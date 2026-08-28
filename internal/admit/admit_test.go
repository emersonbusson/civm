package admit

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/memwatchdog"
)

// fakeSlots models the kernel's per-path advisory flock: at most one holder per
// path. release() frees it. It is the hermetic stand-in for the real
// flock-NB-per-slot mechanism — no syscall, no /run/civm.
type fakeSlots struct {
	mu    sync.Mutex
	held  map[string]bool
	files map[string][]byte // slot path -> recorded JSON record
}

func newFakeSlots() *fakeSlots {
	return &fakeSlots{held: map[string]bool{}, files: map[string][]byte{}}
}

// flock returns a release fn on success, or an error mimicking EWOULDBLOCK when
// the path is already held (contention). The returned release is idempotent.
func (f *fakeSlots) flock(path string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[path] {
		return nil, errSlotBusy // the package's contention sentinel: grabFreeSlot skips, not fails
	}
	f.held[path] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.held[path] = false
		})
	}, nil
}

func (f *fakeSlots) readFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[path]
	if !ok {
		return nil, os.ErrNotExist // readSlotRecord reads this as "no record yet", not an error
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeSlots) writeFile(path string, data []byte, _ uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = append([]byte(nil), data...)
	return nil
}

// seedRecord pre-writes a slot record (e.g. an orphaned holder's unit) so reap
// tests can assert the unit is stopped before reuse.
func (f *fakeSlots) seedRecord(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = append([]byte(nil), data...)
}

func (f *fakeSlots) isHeld(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[path]
}

var errFakeCheck = errors.New("meminfo read failed")

// recorder captures RunFn invocations so tests assert systemctl stop is called
// exactly when expected (reap / Release co-termination).
type recorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recorder) run(name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil, nil
}

func (r *recorder) stopCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if isStopCall(c) {
			n++
		}
	}
	return n
}

func (r *recorder) stoppedUnit(unit string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if isStopCall(c) && c[len(c)-1] == unit {
			return true
		}
	}
	return false
}

// isStopCall matches the co-termination invocation `sudo systemctl stop -- <unit>`
// (DT-v3-2: root stop, with `--` guarding the unit against option injection).
func isStopCall(c []string) bool {
	return len(c) == 5 && c[0] == "sudo" && c[1] == "systemctl" && c[2] == "stop" && c[3] == "--"
}

// baseOpts wires a hermetic admission harness: in-process slots, an OK
// watchdog, a recording RunFn, all PIDs alive, a fixed clock and a no-op sleep.
func baseOpts(t *testing.T, slots *fakeSlots, rec *recorder) Options {
	t.Helper()
	return Options{
		MaxHeavy:        2,
		HeavyMaxMB:      0,
		SlotPrefix:      "/run/civm/admit-heavy-",
		DockerSlotPath:  "/run/civm/admit-docker.lock",
		Weight:          WeightHeavy,
		WaitBudget:      2 * time.Second,
		Backoff:         time.Millisecond,
		FlockFn:         slots.flock,
		ReadFileFn:      slots.readFile,
		WriteFileFn:     slots.writeFile,
		CheckFn:         func() (memwatchdog.Decision, error) { return memwatchdog.DecisionOK, nil },
		RunFn:           rec.run,
		PidAliveFn:      func(int) bool { return true },
		PidStartTicksFn: func(int) (uint64, error) { return 7, nil },
		NowFn:           func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		SleepFn:         func(time.Duration) {},
	}
}

func TestAcquireLightReturnsImmediatelyNoSlot(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	opts.Weight = WeightLight
	// Saturate both heavy slots: a light job must still pass with no slot.
	r1, _ := slots.flock("/run/civm/admit-heavy-1.lock")
	r2, _ := slots.flock("/run/civm/admit-heavy-2.lock")
	defer r1()
	defer r2()

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("light Acquire err = %v", err)
	}
	if adm == nil {
		t.Fatalf("light Acquire returned nil admission")
	}
	if adm.HoldsSlot() {
		t.Fatalf("light admission must not hold a heavy slot")
	}
	if err := adm.Release(); err != nil {
		t.Fatalf("light Release err = %v", err)
	}
}

func TestAcquireHeavyTakesFreeSlot(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("heavy Acquire err = %v", err)
	}
	if !adm.HoldsSlot() {
		t.Fatalf("heavy admission must hold a slot")
	}
	if adm.SlotPath() != "/run/civm/admit-heavy-1.lock" {
		t.Fatalf("heavy took slot %q, want admit-heavy-1.lock", adm.SlotPath())
	}
	if !slots.isHeld("/run/civm/admit-heavy-1.lock") {
		t.Fatalf("slot 1 not held after Acquire")
	}
	if err := adm.Release(); err != nil {
		t.Fatalf("Release err = %v", err)
	}
	if slots.isHeld("/run/civm/admit-heavy-1.lock") {
		t.Fatalf("slot 1 still held after Release")
	}
}

func TestAcquireSecondHeavyTakesSecondSlot(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)

	first, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Acquire err = %v", err)
	}
	defer func() { _ = first.Release() }()
	second, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Acquire err = %v", err)
	}
	defer func() { _ = second.Release() }()
	if first.SlotPath() == second.SlotPath() {
		t.Fatalf("two heavy jobs took the same slot %q", first.SlotPath())
	}
}

func TestAcquireThirdHeavyTimesOutNoExtraSlot(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	opts.WaitBudget = 30 * time.Millisecond
	// Occupy both heavy slots out-of-band so Acquire never finds a free one.
	r1, _ := slots.flock("/run/civm/admit-heavy-1.lock")
	r2, _ := slots.flock("/run/civm/admit-heavy-2.lock")
	defer r1()
	defer r2()

	// NowFn must advance so the WaitBudget deadline is actually reached.
	start := time.Unix(1_700_000_000, 0).UTC()
	var mu sync.Mutex
	cur := start
	opts.NowFn = func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	opts.SleepFn = func(d time.Duration) { mu.Lock(); cur = cur.Add(d); mu.Unlock() }
	opts.Backoff = 5 * time.Millisecond

	_, err := Acquire(context.Background(), opts)
	if !errors.Is(err, ErrWaitBudgetExceeded) {
		t.Fatalf("third heavy Acquire err = %v, want ErrWaitBudgetExceeded", err)
	}
	// No N+1 slot was minted: only the two seeded slots exist as held.
	for _, p := range []string{"/run/civm/admit-heavy-3.lock"} {
		if slots.isHeld(p) {
			t.Fatalf("Acquire minted an extra slot %q", p)
		}
	}
}

func TestAcquireHeavyRefusesOnCritical(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	opts.WaitBudget = 30 * time.Millisecond
	opts.CheckFn = func() (memwatchdog.Decision, error) { return memwatchdog.DecisionCritical, nil }

	start := time.Unix(1_700_000_000, 0).UTC()
	var mu sync.Mutex
	cur := start
	opts.NowFn = func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	opts.SleepFn = func(d time.Duration) { mu.Lock(); cur = cur.Add(d); mu.Unlock() }
	opts.Backoff = 5 * time.Millisecond

	_, err := Acquire(context.Background(), opts)
	if !errors.Is(err, ErrWaitBudgetExceeded) {
		t.Fatalf("Critical Acquire err = %v, want backoff→ErrWaitBudgetExceeded", err)
	}
	// Critical means it never took a slot.
	if slots.isHeld("/run/civm/admit-heavy-1.lock") {
		t.Fatalf("Critical admitted a heavy job into a slot")
	}
}

func TestAcquireHeavyBacksOffOnWarn(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	opts.WaitBudget = 30 * time.Millisecond
	opts.CheckFn = func() (memwatchdog.Decision, error) { return memwatchdog.DecisionWarn, nil }

	start := time.Unix(1_700_000_000, 0).UTC()
	var mu sync.Mutex
	cur := start
	opts.NowFn = func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	opts.SleepFn = func(d time.Duration) { mu.Lock(); cur = cur.Add(d); mu.Unlock() }
	opts.Backoff = 5 * time.Millisecond

	_, err := Acquire(context.Background(), opts)
	if !errors.Is(err, ErrWaitBudgetExceeded) {
		t.Fatalf("Warn heavy Acquire err = %v, want no-new-heavy→timeout", err)
	}
	if slots.isHeld("/run/civm/admit-heavy-1.lock") {
		t.Fatalf("Warn admitted a new heavy job")
	}
}

func TestAcquireFailsClosedOnCheckError(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	opts.WaitBudget = 30 * time.Millisecond
	opts.CheckFn = func() (memwatchdog.Decision, error) { return memwatchdog.DecisionCritical, errFakeCheck }

	start := time.Unix(1_700_000_000, 0).UTC()
	var mu sync.Mutex
	cur := start
	opts.NowFn = func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	opts.SleepFn = func(d time.Duration) { mu.Lock(); cur = cur.Add(d); mu.Unlock() }
	opts.Backoff = 5 * time.Millisecond

	_, err := Acquire(context.Background(), opts)
	if err == nil {
		t.Fatalf("CheckFn error must NOT admit (fail-closed); got nil err")
	}
	if slots.isHeld("/run/civm/admit-heavy-1.lock") {
		t.Fatalf("fail-open: admitted a heavy job despite CheckFn error")
	}
}

func TestReleaseIdempotentAndStopsUnitOnce(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	// Acquire reserved a deterministic unit name and persisted it in the record
	// before any start — no SetUnit, no stderr scrape (C1).
	unit := adm.UnitName()
	if unit == "" {
		t.Fatalf("heavy admission has no reserved unit name")
	}

	if err := adm.Release(); err != nil {
		t.Fatalf("first Release err = %v", err)
	}
	if err := adm.Release(); err != nil {
		t.Fatalf("second Release (idempotent) err = %v", err)
	}
	if got := rec.stopCalls(); got != 1 {
		t.Fatalf("systemctl stop called %d times, want exactly 1", got)
	}
	if !rec.stoppedUnit(unit) {
		t.Fatalf("Release did not stop the recorded unit %q", unit)
	}
}

func TestReleaseLightDoesNotCallSystemctl(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	opts.Weight = WeightLight

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	// A light admission holds no slot and reserves no unit; Release must not call
	// systemctl stop on an empty unit.
	if adm.UnitName() != "" {
		t.Fatalf("light admission unexpectedly reserved a unit %q", adm.UnitName())
	}
	if err := adm.Release(); err != nil {
		t.Fatalf("Release err = %v", err)
	}
	if got := rec.stopCalls(); got != 0 {
		t.Fatalf("systemctl stop called %d times for a light admission, want 0", got)
	}
}

func TestAcquireReapsOrphanedUnitBeforeReuse(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	// Slot 1 is free (prior admit was SIGKILLed → kernel released the flock) but
	// its record still names a unit whose admit PID (4242) is now dead → orphan.
	slots.seedRecord("/run/civm/admit-heavy-1.lock",
		[]byte(`{"unit":"run-u999.service","pid":4242,"acquired_at":"2026-01-01T00:00:00Z"}`))
	opts.PidAliveFn = func(pid int) bool { return pid != 4242 } // 4242 is dead

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = adm.Release() }()
	if adm.SlotPath() != "/run/civm/admit-heavy-1.lock" {
		t.Fatalf("expected to reuse slot 1, got %q", adm.SlotPath())
	}
	if !rec.stoppedUnit("run-u999.service") {
		t.Fatalf("orphaned unit run-u999.service was NOT reaped before reuse")
	}
}

func TestAcquireDoesNotReapLiveRecordedHolder(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	// Slot 1's record names an admit PID that is still alive AND whose start-ticks
	// match: NOT an orphan. Acquire must not stop a live holder's unit.
	slots.seedRecord("/run/civm/admit-heavy-1.lock",
		[]byte(`{"unit":"run-ulive.service","pid":4243,"pid_start_ticks":7,"acquired_at":"2026-01-01T00:00:00Z"}`))
	opts.PidAliveFn = func(int) bool { return true }                   // recorded PID alive
	opts.PidStartTicksFn = func(int) (uint64, error) { return 7, nil } // and same start-ticks

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = adm.Release() }()
	if rec.stoppedUnit("run-ulive.service") {
		t.Fatalf("reaped a live recorded holder's unit (run-ulive.service)")
	}
}

func TestAcquireReapsRecycledPid(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	// The recorded PID number is alive again — but it was RECYCLED to an unrelated
	// process: its /proc start-ticks (now 7) differ from the record (99). The PID
	// is therefore NOT the original holder, so its orphaned unit must be reaped
	// before reuse — the PID-reuse defense bare kill -0 lacks (C2).
	slots.seedRecord("/run/civm/admit-heavy-1.lock",
		[]byte(`{"unit":"run-urecycled.service","pid":4244,"pid_start_ticks":99,"acquired_at":"2026-01-01T00:00:00Z"}`))
	opts.PidAliveFn = func(int) bool { return true }                   // PID number alive (recycled)
	opts.PidStartTicksFn = func(int) (uint64, error) { return 7, nil } // but a different process

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = adm.Release() }()
	if !rec.stoppedUnit("run-urecycled.service") {
		t.Fatalf("recycled-PID orphan run-urecycled.service was NOT reaped (PID-reuse hole)")
	}
}

func TestAcquireFailsClosedOnNonBusyFlockError(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	// A non-contention flock error (e.g. EACCES on /run/civm) must fail closed,
	// not be masked as "busy" and time out (L2).
	opts.FlockFn = func(string) (func(), error) { return nil, errors.New("permission denied") }

	_, err := Acquire(context.Background(), opts)
	if err == nil || errors.Is(err, ErrWaitBudgetExceeded) {
		t.Fatalf("Acquire err = %v, want a hard (non-timeout) error", err)
	}
}

func TestAcquireProceedsOnCorruptOrphanRecord(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	// A corrupt slot record must not be parsed as a unit to stop; reap logs and
	// proceeds (M3) — the slot is still usable.
	slots.seedRecord("/run/civm/admit-heavy-1.lock", []byte(`{not valid json`))

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = adm.Release() }()
	if rec.stopCalls() != 0 {
		t.Fatalf("a corrupt record triggered %d systemctl stop calls, want 0", rec.stopCalls())
	}
}

func TestReapSkipsInvalidUnitName(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	// A dead holder whose recorded unit name is hostile/invalid must NOT reach the
	// privileged systemctl stop — ValidateServiceUnit rejects it first (B-HIGH).
	slots.seedRecord("/run/civm/admit-heavy-1.lock",
		[]byte(`{"unit":"--signal=SIGKILL","pid":4242,"acquired_at":"2026-01-01T00:00:00Z"}`))
	opts.PidAliveFn = func(pid int) bool { return pid != 4242 } // dead → would reap

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = adm.Release() }()
	if rec.stopCalls() != 0 {
		t.Fatalf("an invalid unit name reached systemctl stop (%d calls), want 0", rec.stopCalls())
	}
}

func TestReapWhenStartTicksUnreadable(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	// The recorded PID is alive but its start-ticks cannot be read (process gone
	// between checks): treat as orphan and reap, fail-safe (C2).
	slots.seedRecord("/run/civm/admit-heavy-1.lock",
		[]byte(`{"unit":"run-ugone.service","pid":4246,"pid_start_ticks":50,"acquired_at":"2026-01-01T00:00:00Z"}`))
	opts.PidAliveFn = func(int) bool { return true }
	opts.PidStartTicksFn = func(int) (uint64, error) { return 0, errors.New("no such process") }

	adm, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire err = %v", err)
	}
	defer func() { _ = adm.Release() }()
	if !rec.stoppedUnit("run-ugone.service") {
		t.Fatalf("orphan with unreadable start-ticks was NOT reaped")
	}
}

func TestAcquireDockerSubSlotSerializesToOne(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	opts.Exclusive = "docker"

	first, err := Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("first docker-exclusive Acquire err = %v", err)
	}
	defer func() { _ = first.Release() }()
	if !slots.isHeld("/run/civm/admit-docker.lock") {
		t.Fatalf("docker sub-slot not held after first Acquire")
	}

	// A second docker-exclusive job must NOT get the docker sub-slot within the
	// budget (count=1). It still grabs a heavy slot, but the docker lock blocks.
	opts2 := baseOpts(t, slots, rec)
	opts2.Exclusive = "docker"
	opts2.WaitBudget = 25 * time.Millisecond
	start := time.Unix(1_700_000_000, 0).UTC()
	var mu sync.Mutex
	cur := start
	opts2.NowFn = func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	opts2.SleepFn = func(d time.Duration) { mu.Lock(); cur = cur.Add(d); mu.Unlock() }
	opts2.Backoff = 5 * time.Millisecond

	_, err = Acquire(context.Background(), opts2)
	if !errors.Is(err, ErrWaitBudgetExceeded) {
		t.Fatalf("second docker-exclusive Acquire err = %v, want ErrWaitBudgetExceeded (docker count=1)", err)
	}
}

func TestAcquireRejectsBadOptions(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{"maxheavy<1", func(o *Options) { o.MaxHeavy = -1 }},
		{"relative slot prefix", func(o *Options) { o.SlotPrefix = "relative-" }},
		{"unknown weight", func(o *Options) { o.Weight = Weight("medium") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			opts := baseOpts(t, slots, rec)
			c.mutate(&opts)
			if _, err := Acquire(context.Background(), opts); err == nil {
				t.Fatalf("Acquire(%s) err = nil, want validation error", c.name)
			}
		})
	}
}

func TestAcquireContextCancel(t *testing.T) {
	t.Parallel()
	slots := newFakeSlots()
	rec := &recorder{}
	opts := baseOpts(t, slots, rec)
	// Fill both slots so Acquire must loop, then cancel the context.
	r1, _ := slots.flock("/run/civm/admit-heavy-1.lock")
	r2, _ := slots.flock("/run/civm/admit-heavy-2.lock")
	defer r1()
	defer r2()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Acquire(ctx, opts)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("Acquire with cancelled ctx err = %v, want context canceled", err)
	}
}
