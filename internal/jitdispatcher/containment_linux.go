//go:build linux

package jitdispatcher

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type CgroupContainment struct {
	// These unexported seams exist only for hermetic tests. Production uses the
	// delegated cgroup v2 root and membership read from /proc.
	root             string
	verifyMembership func(int, string) (bool, error)
}

func (containment CgroupContainment) Prepare(command *exec.Cmd, leaseID string) (io.Closer, error) {
	if command == nil || command.SysProcAttr == nil || !digestRE.MatchString(leaseID) {
		return nil, fmt.Errorf("%w: containment identity is invalid", ErrInvalid)
	}
	child, err := containment.pathForLease(leaseID)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(child, 0o700); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create delegated cgroup: %w", err)
		}
		empty, emptyErr := cgroupEmpty(child)
		if emptyErr != nil || !empty {
			return nil, errors.Join(fmt.Errorf("stale delegated cgroup requires recovery"), emptyErr)
		}
		if removeErr := os.Remove(child); removeErr != nil {
			return nil, fmt.Errorf("remove stale empty cgroup: %w", removeErr)
		}
		if createErr := os.Mkdir(child, 0o700); createErr != nil {
			return nil, fmt.Errorf("recreate delegated cgroup: %w", createErr)
		}
	}
	file, err := os.Open(child)
	if err != nil {
		_ = os.Remove(child)
		return nil, fmt.Errorf("open delegated cgroup: %w", err)
	}
	command.SysProcAttr.UseCgroupFD = true
	command.SysProcAttr.CgroupFD = int(file.Fd())
	return file, nil
}

func (containment CgroupContainment) Attach(pid int, leaseID string) (ProcessIdentity, error) {
	if pid <= 1 || !digestRE.MatchString(leaseID) {
		return ProcessIdentity{}, fmt.Errorf("%w: containment identity is invalid", ErrInvalid)
	}
	start, err := processStartTicks(pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("read process identity: %w", err)
	}
	child, err := containment.pathForLease(leaseID)
	if err != nil {
		return ProcessIdentity{}, err
	}
	verifyMembership := processInCgroup
	if containment.verifyMembership != nil {
		verifyMembership = containment.verifyMembership
	}
	inCgroup, err := verifyMembership(pid, child)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("verify process cgroup: %w", err)
	}
	if !inCgroup {
		return ProcessIdentity{}, fmt.Errorf("process was not born in its delegated cgroup")
	}
	processGroup, err := syscall.Getpgid(pid)
	if err != nil || processGroup != pid {
		return ProcessIdentity{}, errors.Join(fmt.Errorf("process group identity is invalid"), err)
	}
	after, err := processStartTicks(pid)
	if err != nil || after != start {
		return ProcessIdentity{}, fmt.Errorf("process identity changed during cgroup verification")
	}
	return ProcessIdentity{PID: pid, StartTicks: start, ProcessGroup: processGroup, CgroupPath: child}, nil
}

func processInCgroup(pid int, expected string) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "0::/") {
			continue
		}
		relative := strings.TrimPrefix(line, "0::/")
		actual := filepath.Clean(filepath.Join("/sys/fs/cgroup", relative))
		return actual == expected || strings.HasPrefix(actual, expected+string(filepath.Separator)), nil
	}
	return false, fmt.Errorf("unified cgroup identity is unavailable")
}

func (CgroupContainment) Alive(identity ProcessIdentity) (bool, error) {
	if err := validateProcessIdentity(identity); err != nil {
		return false, err
	}
	return processIdentityAlive(identity.PID, identity.StartTicks)
}

func (CgroupContainment) Terminate(ctx context.Context, identity ProcessIdentity, grace time.Duration) error {
	if err := validateProcessIdentity(identity); err != nil {
		return err
	}
	return terminateContainment(ctx, identity, identity.CgroupPath, grace)
}

func (containment CgroupContainment) Recover(
	ctx context.Context,
	leaseID string,
	identity ProcessIdentity,
	grace time.Duration,
) error {
	expectedPath, err := containment.pathForLease(leaseID)
	if err != nil {
		return err
	}
	if identity.PID == 0 {
		if identity.StartTicks != 0 || identity.ProcessGroup != 0 || identity.CgroupPath != "" {
			return fmt.Errorf("%w: partial recovery process identity", ErrInvalid)
		}
	} else {
		if err := validateProcessIdentity(identity); err != nil {
			return err
		}
		if identity.CgroupPath != expectedPath {
			return fmt.Errorf("%w: recovery cgroup does not match lease", ErrInvalid)
		}
	}
	return terminateContainment(ctx, identity, expectedPath, grace)
}

func terminateContainment(
	ctx context.Context,
	identity ProcessIdentity,
	cgroupPath string,
	grace time.Duration,
) error {
	if grace <= 0 || grace > time.Minute {
		return fmt.Errorf("%w: containment grace is invalid", ErrInvalid)
	}
	var result error
	if identity.PID > 0 {
		alive, err := processIdentityAlive(identity.PID, identity.StartTicks)
		if err != nil {
			return err
		}
		if alive {
			result = errors.Join(result, signalExactProcessGroup(identity, false))
		}
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		gone, checkErr := containmentGone(identity, cgroupPath)
		if checkErr != nil {
			return errors.Join(result, checkErr)
		}
		if gone {
			return result
		}
		select {
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		case <-deadline.C:
			if identity.PID > 0 {
				result = errors.Join(result, signalExactProcessGroup(identity, true))
			}
			if killErr := os.WriteFile(filepath.Join(cgroupPath, "cgroup.kill"), []byte("1"), 0o600); killErr != nil && !os.IsNotExist(killErr) {
				result = errors.Join(result, fmt.Errorf("kill cgroup: %w", killErr))
			}
			for attempt := 0; attempt < 80; attempt++ {
				gone, checkErr = containmentGone(identity, cgroupPath)
				if checkErr != nil {
					return errors.Join(result, checkErr)
				}
				if gone {
					return result
				}
				select {
				case <-ctx.Done():
					return errors.Join(result, ctx.Err())
				case <-time.After(25 * time.Millisecond):
				}
			}
			return errors.Join(result, fmt.Errorf("driver process or cgroup survived forced termination"))
		case <-ticker.C:
		}
	}
}

func containmentGone(identity ProcessIdentity, cgroupPath string) (bool, error) {
	empty, err := cgroupEmpty(cgroupPath)
	if err != nil {
		return false, err
	}
	alive := false
	if identity.PID > 0 {
		alive, err = processIdentityAlive(identity.PID, identity.StartTicks)
		if err != nil {
			return false, err
		}
	}
	if !empty || alive {
		return false, nil
	}
	if err := os.Remove(cgroupPath); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}

func cgroupPathForLease(leaseID string) (string, error) {
	return (CgroupContainment{}).pathForLease(leaseID)
}

func (containment CgroupContainment) pathForLease(leaseID string) (string, error) {
	if !digestRE.MatchString(leaseID) {
		return "", fmt.Errorf("%w: cgroup lease identity is invalid", ErrInvalid)
	}
	root := containment.root
	if root == "" {
		var err error
		root, err = delegatedCgroupRoot()
		if err != nil {
			return "", err
		}
	} else if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", fmt.Errorf("%w: injected cgroup root is invalid", ErrInvalid)
	}
	return filepath.Join(root, "civm-jit-"+leaseID), nil
}

func validateProcessIdentity(identity ProcessIdentity) error {
	if identity.PID <= 1 || identity.StartTicks == 0 || identity.ProcessGroup != identity.PID ||
		!filepath.IsAbs(identity.CgroupPath) || filepath.Clean(identity.CgroupPath) != identity.CgroupPath ||
		!strings.HasPrefix(filepath.Base(identity.CgroupPath), "civm-jit-") {
		return fmt.Errorf("%w: process containment identity is invalid", ErrInvalid)
	}
	return nil
}

func delegatedCgroupRoot() (string, error) {
	file, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read self cgroup: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var relative string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "0::/") || line == "0::/" {
			relative = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if relative == "" || !filepath.IsAbs(relative) || strings.Contains(relative, "..") {
		return "", fmt.Errorf("cgroup v2 delegation is unavailable")
	}
	root := filepath.Join("/sys/fs/cgroup", relative)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("delegated cgroup root is unavailable")
	}
	return filepath.Clean(root), nil
}

func cgroupEmpty(path string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] == "0", nil
		}
	}
	return false, fmt.Errorf("cgroup.events lacks populated state")
}

func signalExactProcessGroup(identity ProcessIdentity, force bool) error {
	alive, err := processIdentityAlive(identity.PID, identity.StartTicks)
	if err != nil || !alive {
		return err
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-identity.ProcessGroup, signal); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
