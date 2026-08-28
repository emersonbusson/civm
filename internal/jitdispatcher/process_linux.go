//go:build linux

package jitdispatcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func configurePersistentProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalProcessGroup(pid int, force bool) error {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-pid, signal); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("signal runner process group: %w", err)
	}
	return nil
}

func currentProcessStart() (uint64, error) {
	return processStartTicks(os.Getpid())
}

func processStartTicks(pid int) (uint64, error) {
	if pid <= 1 {
		return 0, fmt.Errorf("process PID is invalid")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	closing := strings.LastIndexByte(string(data), ')')
	if closing < 0 || closing+2 >= len(data) {
		return 0, fmt.Errorf("process stat is malformed")
	}
	fields := strings.Fields(string(data[closing+2:]))
	// Field 22 (starttime) becomes index 19 after removing pid/comm.
	if len(fields) <= 19 {
		return 0, fmt.Errorf("process stat is incomplete")
	}
	value, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("process start identity is invalid")
	}
	return value, nil
}

func processIdentityAlive(pid int, start uint64) (bool, error) {
	current, err := processStartTicks(pid)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return current == start, nil
}

func terminateExactProcess(ctxDone <-chan struct{}, pid int, start uint64, grace time.Duration) error {
	alive, err := processIdentityAlive(pid, start)
	if err != nil || !alive {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("terminate exact process: %w", err)
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		alive, checkErr := processIdentityAlive(pid, start)
		if checkErr != nil {
			return checkErr
		}
		if !alive {
			return nil
		}
		select {
		case <-ctxDone:
			return fmt.Errorf("process termination canceled")
		case <-deadline.C:
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return fmt.Errorf("kill exact process: %w", err)
			}
			for attempt := 0; attempt < 40; attempt++ {
				alive, checkErr = processIdentityAlive(pid, start)
				if checkErr != nil || !alive {
					return checkErr
				}
				time.Sleep(25 * time.Millisecond)
			}
			return fmt.Errorf("process survived SIGKILL")
		case <-ticker.C:
		}
	}
}

func openNoFollow(path string) (*os.File, error) {
	return openFileNoFollow(path, os.O_RDONLY, 0)
}

func openFileNoFollow(path string, flags int, mode os.FileMode) (*os.File, error) {
	descriptor, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = syscall.Close(descriptor)
		return nil, fmt.Errorf("file allocation failed")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("source is not a regular file")
	}
	return file, nil
}
