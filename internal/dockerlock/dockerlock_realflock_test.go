package dockerlock

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// TestRealFlockSerializesAcquire exercises Acquire/Release against the REAL
// syscall.Flock (not the in-memory fakeFlock model) on a real temp lock file,
// proving the OS primitive the whole package rests on actually serializes two
// holders. Without it, every other dockerlock test validates only the logic
// around the fake, never that a real advisory flock delivers the exclusion the
// fake assumes — the discipline-#13 gap (a privileged coordination primitive
// proven only against a no-op model, never the real race).
func TestRealFlockSerializesAcquire(t *testing.T) {
	e := newEnv(t)
	opts := e.options()
	opts.FlockFn = syscall.Flock // the real OS advisory lock, not e.flock.flock
	opts.NowFn = time.Now        // real clock — the frozen env clock never lets WaitBudget expire
	opts.WaitBudget = 150 * time.Millisecond

	ctx := context.Background()

	a, err := Acquire(ctx, opts)
	if err != nil {
		t.Fatalf("first Acquire should hold the real flock: %v", err)
	}

	// While A holds the real flock, a second holder MUST be blocked by the OS.
	if b, err := Acquire(ctx, opts); err == nil {
		_ = b.Release()
		_ = a.Release()
		t.Fatal("second Acquire succeeded while the first held the real flock")
	}

	// Once A releases, the real flock is free and B can take it — proving the
	// lock is genuinely held and genuinely released, not just modeled.
	if err := a.Release(); err != nil {
		t.Fatalf("release A: %v", err)
	}

	b, err := Acquire(ctx, opts)
	if err != nil {
		t.Fatalf("Acquire after release should succeed: %v", err)
	}
	if err := b.Release(); err != nil {
		t.Fatalf("release B: %v", err)
	}
}
