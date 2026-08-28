package admit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/emersonbusson/civm/internal/memwatchdog"
)

// backoffStep is the linear backoff between heavy-slot attempts. No exponential:
// the WaitBudget bounds total wait and the deadline is the real timeout.
const backoffStep = 250 * time.Millisecond

// runDirMode is the mode used to create the parent dir of a slot (e.g. /run/civm).
const runDirMode os.FileMode = 0o755

// defaultCheck wraps memwatchdog.Check into the (Decision, error) gate Acquire
// expects. A meminfo read/parse failure surfaces as Critical + error so Acquire
// fails closed (DT-v3-2): it never admits on a broken watchdog read.
func defaultCheck() (memwatchdog.Decision, error) {
	res := memwatchdog.Check(context.Background(), memwatchdog.DefaultOptions())
	if res.Err != "" {
		return memwatchdog.DecisionCritical, errors.New(res.Err)
	}
	return res.Decision, nil
}

// defaultRun runs a command (systemctl stop) with no inherited stdio; output is
// captured so a failing stop is reported, not printed into the job's stream.
func defaultRun(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed systemctl verb + recorded unit name
}

// defaultPidAlive reports whether pid is alive via signal 0 (EPERM means it
// exists but is owned by another user).
func defaultPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// defaultFlockNB takes a non-blocking exclusive advisory lock on path and
// returns an idempotent release that unlocks and closes the descriptor. The
// flock is the liveness signal: the kernel releases it when the holder process
// dies, so a SIGKILLed holder frees the slot for the next Acquire (DT-v3-2,
// no heartbeat). EWOULDBLOCK/EAGAIN (lock busy) is returned as a contention
// error so grabFreeSlot moves to the next slot.
func defaultFlockNB(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), runDirMode); err != nil {
		return nil, fmt.Errorf("admit: criar dir %s: %w", filepath.Dir(path), err)
	}
	// O_NOFOLLOW: refuse to open the slot through a symlink someone pre-planted at
	// the path, so a hostile symlink under /run/civm can't redirect the locked fd.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, slotRecordMode) //nolint:gosec // absolute slot path under /run/civm
	if err != nil {
		return nil, fmt.Errorf("admit: abrir slot %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errSlotBusy
		}
		return nil, fmt.Errorf("admit: flock %s: %w", path, err)
	}
	var released bool
	return func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// errSlotBusy signals slot contention to grabFreeSlot (kept distinct from IO
// errors so a busy slot is skipped, not treated as fatal).
var errSlotBusy = errors.New("admit: slot busy")

// defaultWriteFile persists a slot record atomically enough for an
// already-flocked file: a direct write under the held lock (no concurrent
// writer can hold the same slot). perm is uint32 to match the injected seam.
func defaultWriteFile(path string, data []byte, perm uint32) error {
	return os.WriteFile(path, data, os.FileMode(perm))
}
