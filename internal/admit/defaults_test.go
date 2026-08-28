package admit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/memwatchdog"
)

// These tests exercise the REAL production default implementations (flock,
// signal-0 liveness, file write, meminfo check) against the live OS / temp
// files — not the hermetic gating path. They are the admit analog of
// portblock's TestRealFlockSerializes... and dockerlock's parseStartTicks tests.

func TestDefaultFlockNBExcludesSecondHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "admit-heavy-1.lock")
	release, err := defaultFlockNB(path)
	if err != nil {
		t.Fatalf("first defaultFlockNB err = %v", err)
	}
	// A second non-blocking flock on the same path must report contention.
	if _, err2 := defaultFlockNB(path); err2 == nil {
		t.Fatalf("second defaultFlockNB succeeded while first held the lock")
	}
	// Release is idempotent and frees the lock for a fresh holder.
	release()
	release() // no-op
	again, err := defaultFlockNB(path)
	if err != nil {
		t.Fatalf("defaultFlockNB after release err = %v (lock leaked?)", err)
	}
	again()
}

func TestDefaultFlockNBCreatesParentDir(t *testing.T) {
	t.Parallel()
	nested := filepath.Join(t.TempDir(), "run", "civm", "admit-heavy-1.lock")
	release, err := defaultFlockNB(nested)
	if err != nil {
		t.Fatalf("defaultFlockNB nested err = %v", err)
	}
	defer release()
	if _, statErr := os.Stat(nested); statErr != nil {
		t.Fatalf("slot file not created: %v", statErr)
	}
}

func TestDefaultPidAlive(t *testing.T) {
	t.Parallel()
	if !defaultPidAlive(os.Getpid()) {
		t.Fatalf("defaultPidAlive(self) = false, want true")
	}
	if defaultPidAlive(-1) {
		t.Fatalf("defaultPidAlive(-1) = true, want false")
	}
	// A very large PID is almost certainly not alive.
	if defaultPidAlive(1 << 30) {
		t.Fatalf("defaultPidAlive(huge) = true, want false")
	}
}

func TestDefaultWriteFileRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "slot.lock")
	if err := defaultWriteFile(path, []byte(`{"unit":"x"}`), uint32(slotRecordMode)); err != nil {
		t.Fatalf("defaultWriteFile err = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback err = %v", err)
	}
	if string(got) != `{"unit":"x"}` {
		t.Fatalf("roundtrip = %q", string(got))
	}
}

func TestDefaultCheckReadsRealMeminfo(t *testing.T) {
	t.Parallel()
	// On the CI/dev box /proc/meminfo exists, so defaultCheck returns a real
	// decision with no error. We only assert the no-error contract; the exact
	// decision depends on live memory.
	dec, err := defaultCheck()
	if err != nil {
		t.Fatalf("defaultCheck err = %v on a box with /proc/meminfo", err)
	}
	switch dec {
	case memwatchdog.DecisionOK, memwatchdog.DecisionWarn, memwatchdog.DecisionCritical:
	default:
		t.Fatalf("defaultCheck decision = %v, out of range", dec)
	}
}

func TestDefaultCheckFailsClosedOnBadMeminfo(t *testing.T) {
	t.Parallel()
	// Drive memwatchdog.Check directly with a failing reader to prove the
	// fail-closed mapping (Err != "" → Critical + error) the default relies on.
	res := memwatchdog.Check(context.Background(), memwatchdog.Options{
		MeminfoFn: func() (string, error) { return "", os.ErrPermission },
		NowFn:     func() time.Time { return time.Unix(0, 0) },
	})
	if res.Err == "" || res.Decision != memwatchdog.DecisionCritical {
		t.Fatalf("memwatchdog.Check on read failure = %+v, want Critical+Err", res)
	}
}

// TestApplyDefaultsFillsProductionSeams asserts a zero-valued Options gets every
// effectful seam wired (so a real caller that only sets Weight still runs).
func TestApplyDefaultsFillsProductionSeams(t *testing.T) {
	t.Parallel()
	opts := Options{Weight: WeightHeavy}
	applyDefaults(&opts)
	if opts.MaxHeavy < 1 || opts.SlotPrefix == "" || opts.DockerSlotPath == "" {
		t.Fatalf("config defaults not filled: %+v", opts)
	}
	if opts.WaitBudget <= 0 || opts.Backoff <= 0 {
		t.Fatalf("timing defaults not filled: wait=%v backoff=%v", opts.WaitBudget, opts.Backoff)
	}
	if opts.NowFn == nil || opts.SleepFn == nil || opts.CheckFn == nil ||
		opts.RunFn == nil || opts.PidAliveFn == nil || opts.PidStartTicksFn == nil ||
		opts.FlockFn == nil || opts.ReadFileFn == nil || opts.WriteFileFn == nil {
		t.Fatalf("effectful seam left nil after applyDefaults: %+v", opts)
	}
}
