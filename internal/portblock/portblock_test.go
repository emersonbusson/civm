package portblock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	slotCmpx  = "cmpx"
	slotPeer = "peer"
	slotApp = "acme"
)

// noopFlock is a stand-in for syscall.Flock so unit tests never touch a real
// advisory lock.
func noopFlock(_ int, _ int) error { return nil }

// tempOptions wires Allocate against a real temp StatePath (so os.Rename works)
// with a no-op flock. ReadFile/WriteFile stay real to exercise the round-trip.
func tempOptions(t *testing.T) Options {
	t.Helper()
	opts := DefaultOptions()
	opts.StatePath = filepath.Join(t.TempDir(), "port-blocks.json")
	opts.FlockFn = noopFlock
	return opts
}

func TestAllocateDistinctSlotsGetDisjointBases(t *testing.T) {
	opts := tempOptions(t)

	baseCmpx, err := Allocate(opts, slotCmpx)
	if err != nil {
		t.Fatalf("Allocate(%q): %v", slotCmpx, err)
	}
	basePeer, err := Allocate(opts, slotPeer)
	if err != nil {
		t.Fatalf("Allocate(%q): %v", slotPeer, err)
	}
	baseApp, err := Allocate(opts, slotApp)
	if err != nil {
		t.Fatalf("Allocate(%q): %v", slotApp, err)
	}

	bases := []int{baseCmpx, basePeer, baseApp}
	seen := map[int]string{}
	for i, base := range bases {
		if base < opts.BlockStart || base+opts.BlockSize > opts.WindowEnd {
			t.Fatalf("base %d out of window [%d,%d)", base, opts.BlockStart, opts.WindowEnd)
		}
		if (base-opts.BlockStart)%opts.BlockSize != 0 {
			t.Fatalf("base %d is not aligned to step %d from start %d", base, opts.BlockSize, opts.BlockStart)
		}
		if prev, dup := seen[base]; dup {
			t.Fatalf("disjoint invariant broken: base %d reused (index %d and %s)", base, i, prev)
		}
		seen[base] = slotForIndex(i)
	}

	// Lowest free blocks are handed out in order.
	if baseCmpx != opts.BlockStart {
		t.Fatalf("first base = %d, want %d", baseCmpx, opts.BlockStart)
	}
	if basePeer != opts.BlockStart+opts.BlockSize {
		t.Fatalf("second base = %d, want %d", basePeer, opts.BlockStart+opts.BlockSize)
	}
	if baseApp != opts.BlockStart+2*opts.BlockSize {
		t.Fatalf("third base = %d, want %d", baseApp, opts.BlockStart+2*opts.BlockSize)
	}
}

func slotForIndex(i int) string {
	switch i {
	case 0:
		return slotCmpx
	case 1:
		return slotPeer
	default:
		return slotApp
	}
}

func TestAllocateSameSlotIsSticky(t *testing.T) {
	opts := tempOptions(t)

	first, err := Allocate(opts, slotCmpx)
	if err != nil {
		t.Fatalf("first Allocate: %v", err)
	}
	// Allocate a different slot in between to prove stickiness is by stored
	// base, not by call order.
	if _, err := Allocate(opts, slotPeer); err != nil {
		t.Fatalf("interleaved Allocate: %v", err)
	}
	second, err := Allocate(opts, slotCmpx)
	if err != nil {
		t.Fatalf("second Allocate: %v", err)
	}
	if first != second {
		t.Fatalf("sticky invariant broken: %d != %d", first, second)
	}
}

func TestAllocateWindowExhausted(t *testing.T) {
	opts := tempOptions(t)
	// Tiny window with room for exactly two blocks: [100,228) step 64.
	opts.BlockStart = 100
	opts.BlockSize = 64
	opts.WindowEnd = 228

	if _, err := Allocate(opts, slotCmpx); err != nil {
		t.Fatalf("Allocate first: %v", err)
	}
	if _, err := Allocate(opts, slotPeer); err != nil {
		t.Fatalf("Allocate second: %v", err)
	}
	_, err := Allocate(opts, slotApp)
	if !errors.Is(err, ErrPortWindowExhausted) {
		t.Fatalf("third Allocate err = %v, want ErrPortWindowExhausted", err)
	}
}

func TestAllocateRoundTripJSON(t *testing.T) {
	opts := tempOptions(t)

	want := map[string]int{}
	for _, slot := range []string{slotCmpx, slotPeer, slotApp} {
		base, err := Allocate(opts, slot)
		if err != nil {
			t.Fatalf("Allocate(%q): %v", slot, err)
		}
		want[slot] = base
	}

	data, err := os.ReadFile(opts.StatePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var allocations []Allocation
	if err := json.Unmarshal(data, &allocations); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if len(allocations) != len(want) {
		t.Fatalf("persisted %d allocations, want %d", len(allocations), len(want))
	}
	got := map[string]int{}
	for _, a := range allocations {
		got[a.Slot] = a.Base
	}
	for slot, base := range want {
		if got[slot] != base {
			t.Fatalf("round-trip base for %q = %d, want %d", slot, got[slot], base)
		}
	}

	// A fresh Allocate sees the persisted state and stays sticky.
	reopened := DefaultOptions()
	reopened.StatePath = opts.StatePath
	reopened.FlockFn = noopFlock
	base, err := Allocate(reopened, slotCmpx)
	if err != nil {
		t.Fatalf("reopened Allocate: %v", err)
	}
	if base != want[slotCmpx] {
		t.Fatalf("reopened base for %q = %d, want %d", slotCmpx, base, want[slotCmpx])
	}
}

func TestAllocateDeterministicJSONOrder(t *testing.T) {
	opts := tempOptions(t)
	for _, slot := range []string{slotPeer, slotApp, slotCmpx} {
		if _, err := Allocate(opts, slot); err != nil {
			t.Fatalf("Allocate(%q): %v", slot, err)
		}
	}
	data, err := os.ReadFile(opts.StatePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var allocations []Allocation
	if err := json.Unmarshal(data, &allocations); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	wantOrder := []string{slotApp, slotCmpx, slotPeer} // sorted by slot
	if len(allocations) != len(wantOrder) {
		t.Fatalf("got %d allocations, want %d", len(allocations), len(wantOrder))
	}
	for i, slot := range wantOrder {
		if allocations[i].Slot != slot {
			t.Fatalf("allocations[%d].Slot = %q, want %q", i, allocations[i].Slot, slot)
		}
	}
}

func TestAllocateValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Options)
		slot    string
		wantErr bool
	}{
		{
			name:    "empty slot rejected",
			mutate:  func(*Options) {},
			slot:    "   ",
			wantErr: true,
		},
		{
			name:    "non-positive block size rejected",
			mutate:  func(o *Options) { o.BlockSize = -1 },
			slot:    slotCmpx,
			wantErr: true,
		},
		{
			name:    "window-end not above block-start rejected",
			mutate:  func(o *Options) { o.WindowEnd = o.BlockStart },
			slot:    slotCmpx,
			wantErr: true,
		},
		{
			name:    "valid options accepted",
			mutate:  func(*Options) {},
			slot:    slotCmpx,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tempOptions(t)
			tt.mutate(&opts)
			_, err := Allocate(opts, tt.slot)
			if tt.wantErr && err == nil {
				t.Fatalf("Allocate err = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Allocate err = %v, want nil", err)
			}
		})
	}
}

func TestAllocateInjectsFlockExclusiveThenUnlock(t *testing.T) {
	opts := tempOptions(t)
	var howSeen []int
	opts.FlockFn = func(_ int, how int) error {
		howSeen = append(howSeen, how)
		return nil
	}
	if _, err := Allocate(opts, slotCmpx); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(howSeen) != 2 {
		t.Fatalf("flock called %d times, want 2 (LOCK_EX then LOCK_UN)", len(howSeen))
	}
	if howSeen[0] != syscall.LOCK_EX {
		t.Fatalf("first flock how = %d, want LOCK_EX (%d)", howSeen[0], syscall.LOCK_EX)
	}
	if howSeen[1] != syscall.LOCK_UN {
		t.Fatalf("second flock how = %d, want LOCK_UN (%d)", howSeen[1], syscall.LOCK_UN)
	}
}

func TestAllocateFlockErrorPropagates(t *testing.T) {
	opts := tempOptions(t)
	sentinel := errors.New("flock busy")
	opts.FlockFn = func(_ int, how int) error {
		if how == syscall.LOCK_EX {
			return sentinel
		}
		return nil
	}
	_, err := Allocate(opts, slotCmpx)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Allocate err = %v, want wrapped %v", err, sentinel)
	}
}

func TestAllocateCorruptStateReturnsError(t *testing.T) {
	opts := tempOptions(t)
	if err := os.WriteFile(opts.StatePath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}
	if _, err := Allocate(opts, slotCmpx); err == nil {
		t.Fatalf("Allocate err = nil, want parse error")
	}
}

func TestAllocateEmptyStateFileTreatedAsFresh(t *testing.T) {
	opts := tempOptions(t)
	if err := os.WriteFile(opts.StatePath, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("seed empty state: %v", err)
	}
	base, err := Allocate(opts, slotCmpx)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if base != opts.BlockStart {
		t.Fatalf("base = %d, want %d", base, opts.BlockStart)
	}
}
