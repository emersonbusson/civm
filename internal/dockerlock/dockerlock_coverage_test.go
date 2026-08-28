package dockerlock

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	errUnlockMsg = "unlock boom"
	errReadMsg   = "read boom"
)

var (
	errUnlock = errors.New(errUnlockMsg)
	errRead   = errors.New(errReadMsg)
)

// TestWaitedMSAndHoldMSAccessors exercises the WaitedMS/HoldMS accessors, which
// are otherwise never invoked by the behavioral tests.
func TestWaitedMSAndHoldMSAccessors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lock, err := Acquire(context.Background(), e.options())
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = lock.Release() }()

	// WaitedMS is computed from NowFn deltas; with a frozen clock it is 0.
	if got := lock.WaitedMS(); got != 0 {
		t.Fatalf("WaitedMS = %d, want 0 with frozen clock", got)
	}
	// Advance the clock and confirm HoldMS reflects the elapsed hold time.
	e.advance(2 * time.Second)
	if got := lock.HoldMS(); got != int64(2*time.Second/time.Millisecond) {
		t.Fatalf("HoldMS = %d, want %d", got, int64(2*time.Second/time.Millisecond))
	}
}

// TestApplyDefaultsFillsEmptyFields proves every zero-valued Options field is
// replaced by the production default by applyDefaults.
func TestApplyDefaultsFillsEmptyFields(t *testing.T) {
	t.Parallel()
	var opts Options // all zero
	applyDefaults(&opts)

	if opts.LockPath == "" {
		t.Fatalf("LockPath default not applied")
	}
	if opts.HeartbeatPath != opts.LockPath+".hb" {
		t.Fatalf("HeartbeatPath default = %q, want %q", opts.HeartbeatPath, opts.LockPath+".hb")
	}
	if opts.Scope != defaultScope {
		t.Fatalf("Scope default = %q, want %q", opts.Scope, defaultScope)
	}
	if opts.WaitBudget <= 0 || opts.HoldBudget <= 0 || opts.HeartbeatEvery <= 0 {
		t.Fatalf("duration defaults not applied: %+v", opts)
	}
	fns := []struct {
		name string
		ok   bool
	}{
		{"NowFn", opts.NowFn != nil},
		{"FlockFn", opts.FlockFn != nil},
		{"OpenFileFn", opts.OpenFileFn != nil},
		{"ReadFileFn", opts.ReadFileFn != nil},
		{"WriteFileFn", opts.WriteFileFn != nil},
		{"RemoveFn", opts.RemoveFn != nil},
		{"MkdirAllFn", opts.MkdirAllFn != nil},
		{"PidAliveFn", opts.PidAliveFn != nil},
		{"PidStartTicksFn", opts.PidStartTicksFn != nil},
	}
	for _, f := range fns {
		if !f.ok {
			t.Fatalf("default fn %s not applied", f.name)
		}
	}
}

// TestApplyDefaultsKeepsHeartbeatPathWhenLockEmpty covers the HeartbeatPath
// branch where only HeartbeatPath is provided but LockPath is empty.
func TestApplyDefaultsKeepsProvidedHeartbeatPath(t *testing.T) {
	t.Parallel()
	opts := Options{HeartbeatPath: "/custom/hb.json"}
	applyDefaults(&opts)
	if opts.HeartbeatPath != "/custom/hb.json" {
		t.Fatalf("HeartbeatPath = %q, want preserved /custom/hb.json", opts.HeartbeatPath)
	}
}

// TestHolderRepoOnlyAndPIDFallback covers the two Holder branches not hit by
// the happy path: repo-only and the strconv.Itoa(PID) default.
func TestHolderRepoOnlyAndPIDFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hb   Heartbeat
		want string
	}{
		{
			name: "repo only",
			hb: Heartbeat{
				PID: os.Getpid(), PIDStartTicks: selfStartTicks, Scope: testScope,
				Repo: testRepo, ExpiresAt: mustTime("2026-05-29T12:01:30Z"),
			},
			want: testRepo,
		},
		{
			name: "pid fallback when no repo/run",
			hb: Heartbeat{
				PID: os.Getpid(), PIDStartTicks: selfStartTicks, Scope: testScope,
				ExpiresAt: mustTime("2026-05-29T12:01:30Z"),
			},
			want: strconv.Itoa(os.Getpid()),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.setHeartbeat(c.hb)
			if got := Holder(e.options()); got != c.want {
				t.Fatalf("Holder = %q, want %q", got, c.want)
			}
		})
	}
}

// TestHolderEmptyWhenStale confirms Holder resolves to "" when the heartbeat is
// stale (expired), exercising the early-return branch.
func TestHolderEmptyWhenStale(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setHeartbeat(Heartbeat{
		PID: os.Getpid(), PIDStartTicks: selfStartTicks, Scope: testScope,
		Repo: testRepo, RunID: testRunID,
		ExpiresAt: mustTime("2026-05-29T11:00:00Z"), // expired vs frozen now
	})
	if got := Holder(e.options()); got != "" {
		t.Fatalf("Holder = %q, want empty for stale holder", got)
	}
}

// TestDefaultPidAlive exercises the real syscall-backed liveness probe. Our own
// PID is alive; pid<=0 and a never-allocated huge PID are not.
func TestDefaultPidAlive(t *testing.T) {
	t.Parallel()
	if !defaultPidAlive(os.Getpid()) {
		t.Fatalf("defaultPidAlive(self) = false, want true")
	}
	if defaultPidAlive(0) {
		t.Fatalf("defaultPidAlive(0) = true, want false")
	}
	if defaultPidAlive(-1) {
		t.Fatalf("defaultPidAlive(-1) = true, want false")
	}
}

// TestAcquireRejectsNULBytePath covers the NUL-byte branch of validateAbsPath.
func TestAcquireRejectsNULBytePath(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.LockPath = "/run/civm/docker\x00heavy.lock"
	_, err := Acquire(context.Background(), opts)
	if err == nil {
		t.Fatalf("Acquire with NUL byte in lock path must error")
	}
	if !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("err = %v, want NUL-byte message", err)
	}
}

// TestAcquireMkdirAllFailure covers the early MkdirAll error branch.
func TestAcquireMkdirAllFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.MkdirAllFn = func(string, os.FileMode) error { return errFake }
	if _, err := Acquire(context.Background(), opts); err == nil {
		t.Fatalf("Acquire must error when MkdirAll fails")
	}
}

// TestAcquireContextCancelledDuringBackoff drives Acquire into the sleepCtx
// cancellation path: the lock is held, so the contender backs off, and the
// cancelled context aborts the wait without ErrWaitBudgetExceeded.
func TestAcquireContextCancelledDuringBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.NowFn = time.Now
	opts.WaitBudget = 5 * time.Second
	opts.HeartbeatEvery = time.Hour

	first, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Acquire err = %v", err)
	}
	defer func() { _ = first.Release() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the contender runs

	_, err = Acquire(ctx, opts)
	if err == nil {
		t.Fatalf("Acquire with cancelled ctx must error")
	}
	if errors.Is(err, ErrWaitBudgetExceeded) {
		t.Fatalf("err = ErrWaitBudgetExceeded, want context cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestTryAcquireOnceOpenFileFailure covers the OpenFile error branch.
func TestTryAcquireOnceOpenFileFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.OpenFileFn = func(string, int, os.FileMode) (*os.File, error) {
		return nil, errFake
	}
	if _, err := Acquire(context.Background(), opts); err == nil {
		t.Fatalf("Acquire must error when OpenFile fails")
	}
}

// TestTryAcquireOnceNonContentionFlockError covers the fatal (non-EWOULDBLOCK)
// flock error branch: the file is opened then a real flock error is returned.
func TestTryAcquireOnceNonContentionFlockError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.FlockFn = func(fd, how int) error {
		if how&syscall.LOCK_EX != 0 {
			return syscall.EBADF // not contention → fatal
		}
		return nil
	}
	if _, err := Acquire(context.Background(), opts); err == nil {
		t.Fatalf("Acquire must error on non-contention flock failure")
	}
}

// TestWriteHeartbeatPidStartTicksFailure covers the PidStartTicksFn error path
// of writeHeartbeat (reached through Acquire's first heartbeat write) and proves
// the flock is released on failure.
func TestWriteHeartbeatPidStartTicksFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.PidStartTicksFn = func(int) (uint64, error) { return 0, errFake }

	if _, err := Acquire(context.Background(), opts); err == nil {
		t.Fatalf("Acquire must error when pid start-ticks read fails")
	}
	// The flock must have been released, so a clean acquire still succeeds.
	good, gerr := Acquire(context.Background(), e.options())
	if gerr != nil {
		t.Fatalf("follow-up Acquire err = %v (flock leaked?)", gerr)
	}
	if rerr := good.Release(); rerr != nil {
		t.Fatalf("Release err = %v", rerr)
	}
}

// TestUnlockAndCloseFlockError forces LOCK_UN to fail during Release, exercising
// the unlock error branch of unlockAndClose.
func TestUnlockAndCloseFlockError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.FlockFn = func(fd, how int) error {
		if how&syscall.LOCK_UN != 0 {
			return errUnlock
		}
		return e.flock.flock(fd, how)
	}
	lock, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	rerr := lock.Release()
	if rerr == nil {
		t.Fatalf("Release must surface the LOCK_UN error")
	}
	if !errors.Is(rerr, errUnlock) {
		t.Fatalf("Release err = %v, want wrapped %v", rerr, errUnlock)
	}
}

// TestReclaimStaleReadError surfaces a non-NotExist read error from
// readHeartbeat through reclaimStale (and through readHeartbeat's hard-error
// branch).
func TestReclaimStaleReadError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, errRead }

	_, err := ReclaimStale(opts)
	if err == nil {
		t.Fatalf("ReclaimStale must surface a hard read error")
	}
	if !errors.Is(err, errRead) {
		t.Fatalf("err = %v, want wrapped %v", err, errRead)
	}
}

// TestReclaimStaleRemoveError covers the RemoveFn error branch of reclaimStale
// when the heartbeat is stale but removal fails with a non-NotExist error.
func TestReclaimStaleRemoveError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	deadPID := 999200
	e.mu.Lock()
	e.startTick[deadPID] = selfStartTicks
	e.mu.Unlock()
	e.setHeartbeat(Heartbeat{
		PID: deadPID, PIDStartTicks: selfStartTicks, Scope: testScope,
		ExpiresAt: mustTime("2026-05-29T11:00:00Z"),
	})
	opts := e.options()
	opts.RemoveFn = func(string) error { return errFake }

	if _, err := ReclaimStale(opts); err == nil {
		t.Fatalf("ReclaimStale must surface a non-NotExist remove error")
	}
}

// TestReadHeartbeatHardError directly covers the readHeartbeat error branch for
// a non-NotExist read failure.
func TestReadHeartbeatHardError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, errRead }

	_, ok, err := readHeartbeat(opts)
	if err == nil {
		t.Fatalf("readHeartbeat must return a hard error for non-NotExist failure")
	}
	if ok {
		t.Fatalf("readHeartbeat ok = true on error, want false")
	}
}

// TestReadHeartbeatWhitespaceOnly covers the empty-after-trim branch where the
// file exists but holds only whitespace.
func TestReadHeartbeatWhitespaceOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setRawHeartbeat([]byte("   \n\t  "))
	_, ok, err := readHeartbeat(e.options())
	if err != nil {
		t.Fatalf("readHeartbeat err = %v", err)
	}
	if ok {
		t.Fatalf("readHeartbeat ok = true for whitespace-only file, want false")
	}
}

// TestIsActiveHardReadError confirms IsActive propagates a hard read error
// (covering its error-return branch).
func TestIsActiveHardReadError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, errRead }
	if _, err := IsActive(opts); err == nil {
		t.Fatalf("IsActive must surface a hard read error")
	}
}

// TestNextBackoffWithinBounds confirms nextBackoff stays within the documented
// linear window (100ms ± 10ms) across many draws, covering the jitter math.
func TestNextBackoffWithinBounds(t *testing.T) {
	t.Parallel()
	for i := 0; i < 1000; i++ {
		d := nextBackoff()
		if d < backoffStep-backoffJitter || d > backoffStep+backoffJitter {
			t.Fatalf("nextBackoff = %v, out of [%v,%v]", d, backoffStep-backoffJitter, backoffStep+backoffJitter)
		}
	}
}

// TestSleepCtxCompletes covers the timer-fired branch of sleepCtx with a tiny
// duration so the test is fast.
func TestSleepCtxCompletes(t *testing.T) {
	t.Parallel()
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepCtx err = %v, want nil", err)
	}
}

// TestSleepCtxCancelled covers the ctx.Done() branch of sleepCtx.
func TestSleepCtxCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx err = %v, want context.Canceled", err)
	}
}
