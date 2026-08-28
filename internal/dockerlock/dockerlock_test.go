package dockerlock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	testScope      = "docker-heavy"
	testRepo       = "acme/civm"
	testRunID      = "run-42"
	selfStartTicks = uint64(123456)
)

var errFake = errors.New("fake io failure")

// fakeFlock models inter-fd exclusion on a single lock path so that two
// in-process Acquire calls serialize exactly as the OS flock would, without any
// real syscall.
type fakeFlock struct {
	mu    sync.Mutex
	owner int // fd currently holding LOCK_EX, 0 = free
}

func (f *fakeFlock) flock(fd, how int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case how&syscall.LOCK_UN != 0:
		if f.owner == fd {
			f.owner = 0
		}
		return nil
	case how&syscall.LOCK_EX != 0:
		if f.owner != 0 && f.owner != fd {
			return syscall.EWOULDBLOCK
		}
		f.owner = fd
		return nil
	}
	return nil
}

// env wires a fully in-memory dockerlock harness: a real temp lock file (so fd
// is valid for the fake flock), an in-memory heartbeat store, a controllable
// clock, PID liveness and start-ticks.
type env struct {
	t        *testing.T
	lockPath string
	hbPath   string

	flock *fakeFlock

	mu        sync.Mutex
	hbData    []byte
	hbExists  bool
	removed   int
	now       time.Time
	alivePIDs map[int]bool
	startTick map[int]uint64
	startErr  map[int]error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "docker-heavy.lock")
	return &env{
		t:         t,
		lockPath:  lockPath,
		hbPath:    lockPath + ".hb",
		flock:     &fakeFlock{},
		now:       mustTime("2026-05-29T12:00:00Z"),
		alivePIDs: map[int]bool{os.Getpid(): true},
		startTick: map[int]uint64{os.Getpid(): selfStartTicks},
		startErr:  map[int]error{},
	}
}

func mustTime(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm
}

func (e *env) options() Options {
	return Options{
		LockPath:        e.lockPath,
		HeartbeatPath:   e.hbPath,
		Scope:           testScope,
		WaitBudget:      2 * time.Second,
		HoldBudget:      50 * time.Minute,
		HeartbeatEvery:  30 * time.Second,
		Repo:            testRepo,
		RunID:           testRunID,
		NowFn:           e.nowFn,
		FlockFn:         e.flock.flock,
		OpenFileFn:      e.openFile,
		ReadFileFn:      e.readFile,
		WriteFileFn:     e.writeFile,
		RemoveFn:        e.remove,
		MkdirAllFn:      func(string, os.FileMode) error { return nil },
		PidAliveFn:      e.pidAlive,
		PidStartTicksFn: e.pidStartTicks,
	}
}

func (e *env) nowFn() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

func (e *env) openFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm) //nolint:gosec // test-local temp path
}

func (e *env) readFile(name string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if name != e.hbPath || !e.hbExists {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), e.hbData...), nil
}

func (e *env) writeFile(name string, data []byte, _ os.FileMode) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if name != e.hbPath {
		return errFake
	}
	e.hbData = append([]byte(nil), data...)
	e.hbExists = true
	return nil
}

func (e *env) remove(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if name == e.hbPath {
		e.removed++
		if !e.hbExists {
			return os.ErrNotExist
		}
		e.hbExists = false
	}
	return nil
}

func (e *env) setHeartbeat(hb Heartbeat) {
	data, err := json.Marshal(hb)
	if err != nil {
		e.t.Fatalf("marshal heartbeat: %v", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hbData = data
	e.hbExists = true
}

func (e *env) setRawHeartbeat(raw []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hbData = append([]byte(nil), raw...)
	e.hbExists = true
}

func (e *env) pidAlive(pid int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.alivePIDs[pid]
}

func (e *env) pidStartTicks(pid int) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.startErr[pid]; err != nil {
		return 0, err
	}
	ticks, ok := e.startTick[pid]
	if !ok {
		return 0, errFake
	}
	return ticks, nil
}

func TestAcquireWritesFreshHeartbeat(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lock, err := Acquire(context.Background(), e.options())
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			t.Fatalf("Release err = %v", rerr)
		}
	}()

	if lock.Scope() != testScope {
		t.Fatalf("Scope = %q, want %q", lock.Scope(), testScope)
	}
	active, err := IsActive(e.options())
	if err != nil {
		t.Fatalf("IsActive err = %v", err)
	}
	if !active {
		t.Fatalf("IsActive = false, want true after Acquire")
	}
	if got := Holder(e.options()); got != testRepo+"#"+testRunID {
		t.Fatalf("Holder = %q, want %q", got, testRepo+"#"+testRunID)
	}
}

func TestReleaseIsIdempotentAndRemovesHeartbeat(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lock, err := Acquire(context.Background(), e.options())
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("first Release err = %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release (idempotent) err = %v", err)
	}
	active, err := IsActive(e.options())
	if err != nil {
		t.Fatalf("IsActive err = %v", err)
	}
	if active {
		t.Fatalf("IsActive = true after Release, want false")
	}
	if e.removed == 0 {
		t.Fatalf("Release did not remove the heartbeat file")
	}
}

// TestTwoAcquiresSerialize proves the second in-process Acquire blocks while
// the first holds the lock (via the injected fakeFlock) and only succeeds once
// the first releases. Uses -race-safe shared state.
func TestTwoAcquiresSerialize(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// Real time for this test so the WaitBudget backoff actually elapses.
	opts := e.options()
	opts.NowFn = time.Now
	opts.WaitBudget = 3 * time.Second
	opts.HeartbeatEvery = time.Hour // no heartbeat churn during the test

	first, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Acquire err = %v", err)
	}

	acquired := make(chan *Lock, 1)
	errCh := make(chan error, 1)
	go func() {
		l, aerr := Acquire(context.Background(), opts)
		if aerr != nil {
			errCh <- aerr
			return
		}
		acquired <- l
	}()

	// The contender must not acquire while the first holds the lock.
	select {
	case <-acquired:
		t.Fatalf("second Acquire succeeded while first still held the lock")
	case aerr := <-errCh:
		t.Fatalf("second Acquire errored prematurely: %v", aerr)
	case <-time.After(300 * time.Millisecond):
	}

	if rerr := first.Release(); rerr != nil {
		t.Fatalf("first Release err = %v", rerr)
	}

	select {
	case second := <-acquired:
		if rerr := second.Release(); rerr != nil {
			t.Fatalf("second Release err = %v", rerr)
		}
	case aerr := <-errCh:
		t.Fatalf("second Acquire failed after first release: %v", aerr)
	case <-time.After(3 * time.Second):
		t.Fatalf("second Acquire did not proceed after first release")
	}
}

func TestAcquireWaitBudgetExceeded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.NowFn = time.Now
	opts.WaitBudget = 150 * time.Millisecond
	opts.HeartbeatEvery = time.Hour

	first, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Acquire err = %v", err)
	}
	defer func() { _ = first.Release() }()

	_, err = Acquire(context.Background(), opts)
	if !errors.Is(err, ErrWaitBudgetExceeded) {
		t.Fatalf("second Acquire err = %v, want ErrWaitBudgetExceeded", err)
	}
}

func TestIsActiveStaleness(t *testing.T) {
	t.Parallel()
	deadPID := 999001
	reusedPID := 999002
	cases := []struct {
		name string
		hb   Heartbeat
		// setup mutates the env before IsActive.
		setup func(e *env)
		raw   []byte
		want  bool
	}{
		{
			name: "fresh live holder",
			hb: Heartbeat{
				PID: os.Getpid(), PIDStartTicks: selfStartTicks, Scope: testScope,
				ExpiresAt: mustTime("2026-05-29T12:01:30Z"),
			},
			want: true,
		},
		{
			name: "dead pid is stale",
			hb: Heartbeat{
				PID: deadPID, PIDStartTicks: selfStartTicks, Scope: testScope,
				ExpiresAt: mustTime("2026-05-29T12:01:30Z"),
			},
			setup: func(e *env) {
				e.mu.Lock()
				e.startTick[deadPID] = selfStartTicks // ticks resolvable, but PID dead
				e.mu.Unlock()
			},
			want: false,
		},
		{
			name: "reused pid mismatched start-ticks is stale",
			hb: Heartbeat{
				PID: reusedPID, PIDStartTicks: 111, Scope: testScope,
				ExpiresAt: mustTime("2026-05-29T12:01:30Z"),
			},
			setup: func(e *env) {
				e.mu.Lock()
				e.alivePIDs[reusedPID] = true
				e.startTick[reusedPID] = 222 // different from heartbeat's 111
				e.mu.Unlock()
			},
			want: false,
		},
		{
			name: "expired heartbeat is stale",
			hb: Heartbeat{
				PID: os.Getpid(), PIDStartTicks: selfStartTicks, Scope: testScope,
				ExpiresAt: mustTime("2026-05-29T11:59:00Z"), // before now
			},
			want: false,
		},
		{
			name: "corrupt heartbeat is stale",
			raw:  []byte("{not valid json"),
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if c.raw != nil {
				e.setRawHeartbeat(c.raw)
			} else {
				e.setHeartbeat(c.hb)
			}
			if c.setup != nil {
				c.setup(e)
			}
			got, err := IsActive(e.options())
			if err != nil {
				t.Fatalf("IsActive err = %v", err)
			}
			if got != c.want {
				t.Fatalf("IsActive = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsActiveMissingHeartbeat(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	got, err := IsActive(e.options())
	if err != nil {
		t.Fatalf("IsActive err = %v", err)
	}
	if got {
		t.Fatalf("IsActive = true with no heartbeat, want false")
	}
}

func TestReclaimStaleRemovesDeadHolderIdempotently(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	deadPID := 999003
	e.mu.Lock()
	e.startTick[deadPID] = selfStartTicks
	e.mu.Unlock()
	e.setHeartbeat(Heartbeat{
		PID: deadPID, PIDStartTicks: selfStartTicks, Scope: testScope,
		ExpiresAt: mustTime("2026-05-29T12:01:30Z"),
	})

	reclaimed, err := ReclaimStale(e.options())
	if err != nil {
		t.Fatalf("ReclaimStale err = %v", err)
	}
	if !reclaimed {
		t.Fatalf("ReclaimStale = false, want true for dead holder")
	}
	// Idempotent: a second call with no heartbeat still succeeds.
	reclaimedAgain, err := ReclaimStale(e.options())
	if err != nil {
		t.Fatalf("second ReclaimStale err = %v", err)
	}
	if !reclaimedAgain {
		t.Fatalf("second ReclaimStale = false, want true (idempotent)")
	}
}

func TestReclaimStaleLeavesFreshHolder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setHeartbeat(Heartbeat{
		PID: os.Getpid(), PIDStartTicks: selfStartTicks, Scope: testScope,
		ExpiresAt: mustTime("2026-05-29T12:01:30Z"),
	})
	reclaimed, err := ReclaimStale(e.options())
	if err != nil {
		t.Fatalf("ReclaimStale err = %v", err)
	}
	if reclaimed {
		t.Fatalf("ReclaimStale reclaimed a fresh live holder")
	}
	if e.removed != 0 {
		t.Fatalf("fresh holder heartbeat was removed (%d times)", e.removed)
	}
}

func TestOverBudgetMarkedOnceHoldCrossed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.HoldBudget = 40 * time.Minute
	opts.HeartbeatEvery = 10 * time.Millisecond // fast heartbeat for the test clock

	lock, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = lock.Release() }()

	if lock.OverBudget() {
		t.Fatalf("OverBudget = true immediately after Acquire")
	}
	// Push the fake clock past HoldBudget; the heartbeat goroutine flips the
	// alarm on its next tick.
	e.advance(41 * time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lock.OverBudget() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("OverBudget never set after HoldBudget crossed")
}

func TestAcquireReclaimsStaleThenSucceeds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// Pre-seed a stale heartbeat from a dead PID. The flock is free (the dead
	// holder released it), so Acquire takes the lock and overwrites the .hb.
	deadPID := 999004
	e.mu.Lock()
	e.startTick[deadPID] = selfStartTicks
	e.mu.Unlock()
	e.setHeartbeat(Heartbeat{
		PID: deadPID, PIDStartTicks: selfStartTicks, Scope: testScope,
		ExpiresAt: mustTime("2026-05-29T11:00:00Z"),
	})

	lock, err := Acquire(context.Background(), e.options())
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = lock.Release() }()

	active, err := IsActive(e.options())
	if err != nil {
		t.Fatalf("IsActive err = %v", err)
	}
	if !active {
		t.Fatalf("IsActive = false after reclaiming stale and acquiring")
	}
}

func TestAcquireRejectsRelativeLockPath(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.LockPath = "relative/docker-heavy.lock"
	_, err := Acquire(context.Background(), opts)
	if err == nil {
		t.Fatalf("Acquire with relative lock path must error")
	}
}

func TestAcquireWriteHeartbeatFailureUnlocks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opts := e.options()
	opts.WriteFileFn = func(string, []byte, os.FileMode) error { return errFake }

	_, err := Acquire(context.Background(), opts)
	if err == nil {
		t.Fatalf("Acquire must error when heartbeat write fails")
	}
	// The flock must have been released so a subsequent acquire can proceed.
	good, gerr := Acquire(context.Background(), e.options())
	if gerr != nil {
		t.Fatalf("follow-up Acquire err = %v (flock leaked?)", gerr)
	}
	if rerr := good.Release(); rerr != nil {
		t.Fatalf("Release err = %v", rerr)
	}
}

func TestHeartbeatJSONRoundTrips(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lock, err := Acquire(context.Background(), e.options())
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = lock.Release() }()

	hb, ok, err := readHeartbeat(e.options())
	if err != nil {
		t.Fatalf("readHeartbeat err = %v", err)
	}
	if !ok {
		t.Fatalf("readHeartbeat ok = false, want true")
	}
	if hb.PID != os.Getpid() || hb.PIDStartTicks != selfStartTicks {
		t.Fatalf("heartbeat identity mismatch: %+v", hb)
	}
	if hb.Repo != testRepo || hb.RunID != testRunID || hb.Scope != testScope {
		t.Fatalf("heartbeat metadata mismatch: %+v", hb)
	}
}
