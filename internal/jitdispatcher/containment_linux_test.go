//go:build linux

package jitdispatcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCgroupContainmentPreparesFreshAndStaleEmptyLease(t *testing.T) {
	root := t.TempDir()
	containment := CgroupContainment{root: root}
	leaseID := strings.Repeat("a", 64)
	child := filepath.Join(root, "civm-jit-"+leaseID)

	prepare := func() {
		t.Helper()
		command := exec.Command("/bin/true")
		configureProcess(command)
		closer, err := containment.Prepare(command, leaseID)
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if !command.SysProcAttr.UseCgroupFD || command.SysProcAttr.CgroupFD <= 0 {
			t.Fatalf("Prepare() did not configure CLONE_INTO_CGROUP: %+v", command.SysProcAttr)
		}
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}

	prepare()
	prepare()
	if info, err := os.Stat(child); err != nil || !info.IsDir() {
		t.Fatalf("prepared cgroup = %v, %v", info, err)
	}
	if _, err := containment.Prepare(nil, leaseID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Prepare(nil) error = %v", err)
	}

	occupiedLease := strings.Repeat("b", 64)
	occupied := filepath.Join(root, "civm-jit-"+occupiedLease)
	if err := os.Mkdir(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "cgroup.events"), []byte("populated 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/true")
	configureProcess(command)
	if _, err := containment.Prepare(command, occupiedLease); err == nil || !strings.Contains(err.Error(), "requires recovery") {
		t.Fatalf("Prepare() occupied cgroup error = %v", err)
	}
}

func TestCgroupContainmentRecoversWithoutPersistedPID(t *testing.T) {
	root := t.TempDir()
	containment := CgroupContainment{root: root}
	leaseID := strings.Repeat("c", 64)
	child := filepath.Join(root, "civm-jit-"+leaseID)
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := containment.Recover(context.Background(), leaseID, ProcessIdentity{}, 10*time.Millisecond); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Fatalf("Recover() left empty cgroup: %v", err)
	}
	partial := ProcessIdentity{PID: 123}
	if err := containment.Recover(context.Background(), leaseID, partial, 10*time.Millisecond); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Recover() partial identity error = %v", err)
	}
	wrong := ProcessIdentity{
		PID: 1 << 30, StartTicks: 1, ProcessGroup: 1 << 30,
		CgroupPath: filepath.Join(root, "civm-jit-"+strings.Repeat("d", 64)),
	}
	if err := containment.Recover(context.Background(), leaseID, wrong, 10*time.Millisecond); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Recover() wrong lease error = %v", err)
	}
}

func TestCgroupContainmentTerminatesMissingProcessAndEmptyCgroup(t *testing.T) {
	root := t.TempDir()
	leaseID := strings.Repeat("e", 64)
	child := filepath.Join(root, "civm-jit-"+leaseID)
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := ProcessIdentity{
		PID: 1 << 30, StartTicks: 1, ProcessGroup: 1 << 30, CgroupPath: child,
	}
	if err := (CgroupContainment{root: root}).Terminate(context.Background(), identity, 10*time.Millisecond); err != nil {
		t.Fatalf("Terminate() missing process error = %v", err)
	}
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Fatalf("Terminate() left empty cgroup: %v", err)
	}
}

func TestCgroupContainmentAttachVerifiesMembershipAndExactProcessIdentity(t *testing.T) {
	command := exec.Command("/bin/sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
		close(wait)
	}()
	t.Cleanup(func() {
		_ = signalProcessGroup(command.Process.Pid, true)
		select {
		case <-wait:
		case <-time.After(time.Second):
		}
	})
	root := t.TempDir()
	leaseID := strings.Repeat("1", 64)
	expected := filepath.Join(root, "civm-jit-"+leaseID)
	containment := CgroupContainment{
		root: root,
		verifyMembership: func(pid int, path string) (bool, error) {
			return pid == command.Process.Pid && path == expected, nil
		},
	}
	identity, err := containment.Attach(command.Process.Pid, leaseID)
	if err != nil || identity.PID != command.Process.Pid || identity.ProcessGroup != command.Process.Pid || identity.CgroupPath != expected || identity.StartTicks == 0 {
		t.Fatalf("Attach() = %+v, %v", identity, err)
	}
	if _, err := containment.Attach(0, leaseID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Attach() invalid PID error = %v", err)
	}
	refused := CgroupContainment{
		root: root, verifyMembership: func(int, string) (bool, error) { return false, nil },
	}
	if _, err := refused.Attach(command.Process.Pid, leaseID); err == nil {
		t.Fatal("Attach() accepted process outside the lease cgroup")
	}
	failed := CgroupContainment{
		root: root, verifyMembership: func(int, string) (bool, error) { return false, errors.New("proc unavailable") },
	}
	if _, err := failed.Attach(command.Process.Pid, leaseID); err == nil || !strings.Contains(err.Error(), "proc unavailable") {
		t.Fatalf("Attach() membership read error = %v", err)
	}
	if err := signalExactProcessGroup(identity, true); err != nil {
		t.Fatalf("signalExactProcessGroup() error = %v", err)
	}
	if err := <-wait; err == nil {
		t.Fatal("sleep helper unexpectedly exited successfully")
	}
	if err := signalExactProcessGroup(identity, true); err != nil {
		t.Fatalf("signalExactProcessGroup() reaped error = %v", err)
	}
}

func TestCgroupContainmentAliveAndReadOnlyHelpers(t *testing.T) {
	command := exec.Command("/bin/sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
		close(wait)
	}()
	t.Cleanup(func() {
		_ = signalProcessGroup(command.Process.Pid, true)
		select {
		case <-wait:
		case <-time.After(time.Second):
		}
	})
	start, err := processStartTicks(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identity := ProcessIdentity{
		PID: command.Process.Pid, StartTicks: start, ProcessGroup: command.Process.Pid,
		CgroupPath: "/sys/fs/cgroup/civm-jit-" + strings.Repeat("f", 64),
	}
	alive, err := (CgroupContainment{}).Alive(identity)
	if err != nil || !alive {
		t.Fatalf("Alive() = %v, %v", alive, err)
	}
	if err := signalProcessGroup(command.Process.Pid, true); err != nil {
		t.Fatal(err)
	}
	if err := <-wait; err == nil {
		t.Fatal("sleep helper unexpectedly exited successfully")
	}
	alive, err = (CgroupContainment{}).Alive(identity)
	if err != nil || alive {
		t.Fatalf("Alive() after reap = %v, %v", alive, err)
	}

	root, err := delegatedCgroupRoot()
	if err != nil {
		t.Fatalf("delegatedCgroupRoot() error = %v", err)
	}
	inside, err := processInCgroup(os.Getpid(), root)
	if err != nil || !inside {
		t.Fatalf("processInCgroup() = %v, %v for %s", inside, err, root)
	}
	if _, err := cgroupPathForLease("bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cgroupPathForLease() invalid error = %v", err)
	}
	leaseID := strings.Repeat("2", 64)
	path, err := cgroupPathForLease(leaseID)
	if err != nil || filepath.Base(path) != "civm-jit-"+leaseID {
		t.Fatalf("cgroupPathForLease() = %q, %v", path, err)
	}
	if _, err := (CgroupContainment{root: "relative"}).pathForLease(leaseID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pathForLease() relative root error = %v", err)
	}
}

func TestCgroupEmptyParsesUnifiedEvents(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		empty   bool
		wantErr bool
	}{
		{name: "empty", content: "populated 0\n", empty: true},
		{name: "populated", content: "populated 1\n"},
		{name: "missing field", content: "frozen 0\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			empty, err := cgroupEmpty(directory)
			if empty != test.empty || (err != nil) != test.wantErr {
				t.Fatalf("cgroupEmpty() = %v, %v", empty, err)
			}
		})
	}
}
