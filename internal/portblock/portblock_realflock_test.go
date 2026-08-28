package portblock

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestRealFlockSerializesConcurrentAllocate runs many concurrent Allocate calls
// against one StatePath with the REAL syscall.Flock (DefaultOptions), proving
// the exclusive flock actually serializes the read->find->write->rename cycle.
// With a no-op flock (what the other tests inject) the concurrent
// read-modify-write would race: lost updates, duplicate bases, or torn JSON.
// This is the discipline-#13 real exercise of the privileged coordination lock
// (the audit flagged it as proven only against a no-op).
func TestRealFlockSerializesConcurrentAllocate(t *testing.T) {
	opts := DefaultOptions() // real ReadFile/WriteFile/MkdirAll and syscall.Flock
	opts.StatePath = filepath.Join(t.TempDir(), "portblocks.json")

	const n = 8
	bases := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bases[i], errs[i] = Allocate(opts, fmt.Sprintf("slot-%d", i))
		}(i)
	}
	wg.Wait()

	seen := map[int]int{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("slot-%d Allocate failed: %v", i, errs[i])
		}
		if prev, dup := seen[bases[i]]; dup {
			t.Fatalf("base %d handed to slot-%d and slot-%d: real flock did not serialize", bases[i], prev, i)
		}
		seen[bases[i]] = i
	}

	// No lost update: the persisted state must hold every slot. A torn or
	// partially-written file would also fail readState.
	state, err := readState(opts)
	if err != nil {
		t.Fatalf("final state unreadable/torn: %v", err)
	}
	if len(state) != n {
		t.Fatalf("final state has %d slots, want %d (lost update under concurrency)", len(state), n)
	}
}
