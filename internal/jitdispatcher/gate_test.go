//go:build linux

package jitdispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunLeaseHolderRemovesMarkerOnlyOnExplicitRelease(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "lease.json")
	leaseID := strings.Repeat("a", 64)
	admissionID := strings.Repeat("1", 64)
	if err := RunLeaseHolder(strings.NewReader("release\n"), leaseID, admissionID, markerPath); err != nil {
		t.Fatalf("RunLeaseHolder() error = %v", err)
	}
	if _, err := os.Lstat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("explicit release left marker: %v", err)
	}
}

func TestPendingGuardAdmissionIsTerminatedAndReaped(t *testing.T) {
	command := exec.Command("/bin/sleep", "30")
	configurePersistentProcess(command)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	startTicks, err := processStartTicks(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	lease := &execResourceLease{
		command: command, input: input, wait: wait,
		leaseID: strings.Repeat("c", 64), admissionID: strings.Repeat("3", 64),
		path: filepath.Join(t.TempDir(), "missing-marker.json"),
	}
	started := time.Now()
	if err := lease.cancelPending(20 * time.Millisecond); err != nil {
		t.Fatalf("cancelPending() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("pending Guard process was not reaped promptly: %s", elapsed)
	}
	alive, err := processIdentityAlive(command.Process.Pid, startTicks)
	if err != nil || alive {
		t.Fatalf("pending Guard process alive=%v error=%v", alive, err)
	}
}

func TestRunLeaseHolderKeepsMarkerAndProcessOnDispatcherEOF(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "lease.json")
	leaseID := strings.Repeat("b", 64)
	admissionID := strings.Repeat("2", 64)
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- RunLeaseHolder(reader, leaseID, admissionID, markerPath) }()
	waitForLeaseMarker(t, markerPath)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("holder returned on EOF and would release Guard: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	marker, found, err := loadLeaseMarker(markerPath)
	if err != nil || !found || marker.LeaseID != leaseID || marker.AdmissionID != admissionID || marker.HolderPID != os.Getpid() {
		t.Fatalf("durable EOF marker = %+v, found=%v, err=%v", marker, found, err)
	}
}

func TestExecResourceLeaseReleaseAndAbandon(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", `IFS= read -r line; test "$line" = release`)
	configurePersistentProcess(command)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
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
	marker := LeaseMarker{
		Version: 2, LeaseID: strings.Repeat("d", 64), AdmissionID: strings.Repeat("4", 64),
		HolderPID: command.Process.Pid, StartTicks: 1,
	}
	lease := &execResourceLease{
		command: command, input: input, wait: wait, marker: marker,
		leaseID: marker.LeaseID, admissionID: marker.AdmissionID,
		path: filepath.Join(t.TempDir(), "missing-marker.json"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if lease.Marker() != marker || lease.command != nil || lease.input != nil || lease.wait != nil {
		t.Fatalf("released lease state = %+v", lease)
	}
	if err := lease.Release(ctx); err == nil {
		t.Fatal("Release() accepted an inactive lease")
	}

	reader, writer := io.Pipe()
	abandoned := &execResourceLease{input: writer, command: exec.Command("/bin/true")}
	if err := abandoned.Abandon(); err != nil {
		t.Fatalf("Abandon() error = %v", err)
	}
	buffer := make([]byte, 1)
	if _, err := reader.Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("abandoned input read error = %v", err)
	}
	_ = reader.Close()
}

func TestExecResourceGateReleasesOnlyExactOrphan(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "lease.json")
	gate := &ExecResourceGate{SelfExecutable: "/bin/true", MarkerPath: markerPath}
	if _, found, err := gate.Orphan(); err != nil || found {
		t.Fatalf("Orphan() before marker = found:%v err:%v", found, err)
	}

	command := exec.Command("/bin/sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
		close(wait)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-wait:
		case <-time.After(time.Second):
		}
	})
	start, err := processStartTicks(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	marker := LeaseMarker{
		Version: 2, LeaseID: strings.Repeat("e", 64), AdmissionID: strings.Repeat("5", 64),
		HolderPID: command.Process.Pid, StartTicks: start,
	}
	if err := writeLeaseMarker(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := gate.Orphan()
	if err != nil || !found || loaded != marker {
		t.Fatalf("Orphan() = %+v, %v, %v", loaded, found, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate.ReleaseOrphan(ctx, marker, 20*time.Millisecond); err != nil {
		t.Fatalf("ReleaseOrphan() error = %v", err)
	}
	if _, found, err := gate.Orphan(); err != nil || found {
		t.Fatalf("Orphan() after release = found:%v err:%v", found, err)
	}
	select {
	case <-wait:
	case <-time.After(time.Second):
		t.Fatal("orphan holder was not reaped")
	}
	if err := gate.ReleaseOrphan(context.Background(), marker, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ReleaseOrphan() invalid grace error = %v", err)
	}
}

func TestGateRuntimeAndStaleMarkerValidation(t *testing.T) {
	directory := t.TempDir()
	if err := ensureGateRuntimeDirectory(directory); err != nil {
		t.Fatalf("ensureGateRuntimeDirectory() error = %v", err)
	}
	if err := os.Chmod(directory, 0o777); err != nil { //nolint:gosec // test requires permissive directory
		t.Fatal(err)
	}
	if err := ensureGateRuntimeDirectory(directory); err == nil {
		t.Fatal("ensureGateRuntimeDirectory() accepted writable directory")
	}

	markerPath := filepath.Join(t.TempDir(), "stale.json")
	stale := LeaseMarker{
		Version: 2, LeaseID: strings.Repeat("f", 64), AdmissionID: strings.Repeat("6", 64),
		HolderPID: 1 << 30, StartTicks: 1,
	}
	if err := writeLeaseMarker(markerPath, stale); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleLeaseMarker(markerPath, stale); err != nil {
		t.Fatalf("removeStaleLeaseMarker() error = %v", err)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("stale marker remained: %v", err)
	}

	livePath := filepath.Join(t.TempDir(), "live.json")
	start, err := currentProcessStart()
	if err != nil {
		t.Fatal(err)
	}
	live := LeaseMarker{
		Version: 2, LeaseID: strings.Repeat("a", 64), AdmissionID: strings.Repeat("7", 64),
		HolderPID: os.Getpid(), StartTicks: start,
	}
	if err := writeLeaseMarker(livePath, live); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleLeaseMarker(livePath, live); err == nil {
		t.Fatal("removeStaleLeaseMarker() removed a live marker")
	}
	if err := cleanupOwnMarker(livePath, strings.Repeat("b", 64), live.AdmissionID, time.Millisecond); err == nil {
		t.Fatal("cleanupOwnMarker() removed another lease")
	}
}

func TestExecResourceGateRejectsUnsafeSetupBeforeStart(t *testing.T) {
	gate := NewExecResourceGate("/bin/true")
	if gate.SelfExecutable != "/bin/true" || gate.MarkerPath != LeaseMarkerPath {
		t.Fatalf("NewExecResourceGate() = %+v", gate)
	}
	gate.MarkerPath = filepath.Join(t.TempDir(), "lease.json")
	if err := gate.Preflight("/bin/true"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Preflight() configurable marker error = %v", err)
	}
	if _, err := gate.Acquire(context.Background(), "/bin/true", "bad", time.Millisecond, time.Millisecond); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Acquire() invalid lease error = %v", err)
	}
	var nilGate *ExecResourceGate
	if err := nilGate.Preflight("/bin/true"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Preflight() error = %v", err)
	}
	if _, _, err := nilGate.Orphan(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Orphan() error = %v", err)
	}
}

func TestExecResourceGateAcquireRunsThroughGuardAndReleases(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "lease.json")
	holder, _ := writeDriver(t, fmt.Sprintf(`#!/bin/sh
set -eu
test "$1" = "__jit-lease-hold"
test "$2" = "--lease-id"
lease="$3"
test "$4" = "--admission-id"
admission="$5"
marker=%q
start="$(awk '{print $22}' "/proc/$$/stat")"
temporary="${marker}.tmp.$$"
(umask 077; printf '{"version":2,"lease_id":"%%s","admission_id":"%%s","holder_pid":%%s,"start_ticks":%%s}\n' "$lease" "$admission" "$$" "$start" > "$temporary")
mv "$temporary" "$marker"
if IFS= read -r line && test "$line" = "release"; then
  rm -f -- "$marker"
  exit 0
fi
while :; do /bin/sleep 60; done
`, markerPath))
	guard, _ := writeDriver(t, `#!/bin/sh
set -eu
test "$1" = "exec"
shift
test "$1" = "--"
shift
exec "$@"
`)
	gate := &ExecResourceGate{
		SelfExecutable: holder, MarkerPath: markerPath, allowTestMarker: true,
	}
	if err := gate.Preflight(guard); err != nil {
		t.Fatalf("Preflight() hermetic error = %v", err)
	}
	leaseID := strings.Repeat("9", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := gate.Acquire(ctx, guard, leaseID, time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(func() {
		marker, found, _ := loadLeaseMarker(markerPath)
		if found {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
			_ = terminateExactProcess(cleanupCtx.Done(), marker.HolderPID, marker.StartTicks, 20*time.Millisecond)
			cleanupCancel()
			_ = os.Remove(markerPath)
		}
	})
	marker := lease.Marker()
	if marker.LeaseID != leaseID || !digestRE.MatchString(marker.AdmissionID) || marker.HolderPID <= 1 || marker.StartTicks == 0 {
		t.Fatalf("Acquire() marker = %+v", marker)
	}
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), time.Second)
	err = lease.Release(releaseCtx)
	releaseCancel()
	if err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if _, found, err := gate.Orphan(); err != nil || found {
		t.Fatalf("released gate marker = found:%v err:%v", found, err)
	}
}

func waitForLeaseMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, found, err := loadLeaseMarker(path); err == nil && found {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("lease marker was not persisted")
}
