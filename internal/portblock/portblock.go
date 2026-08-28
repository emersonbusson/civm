// Package portblock allocates a deterministic, sticky host-port block per
// runner slot so concurrent multi-project CI jobs never bind the same port.
//
// Each runner slot owns a contiguous block of BlockSize host ports starting at
// a base inside the half-open civm window [BlockStart, WindowEnd). The slot ->
// base map is persisted as JSON in StatePath. Allocate is sticky (re-running
// for the same slot returns the same base) and disjoint (distinct slots never
// share a base).
//
// Concurrency contract: Allocate holds an exclusive flock on a STABLE sidecar
// lock file (StatePath + ".lock") for the ENTIRE read -> find -> write cycle
// and persists StatePath via a temp file plus os.Rename so the on-disk state is
// never torn, even when two processes call Allocate at the same time. The lock
// MUST be the sidecar, never StatePath itself: os.Rename swaps StatePath's
// inode and flock binds to the open inode, so locking StatePath would stop
// serializing the instant the first writer renames — concurrent allocations
// would then race the read -> find -> write cycle and hand the same base to two
// slots (a port-block collision). SPECv2 DT-v2-2/19 specified StatePath; that
// is corrected here, proven by TestRealFlockSerializesConcurrentAllocate. When
// the whole window is occupied it returns ErrPortWindowExhausted, which fails
// the install and signals the operator to remove a dead runner; there is no
// automatic eviction in v1.
package portblock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/emersonbusson/civm/internal/civm"
)

// ErrPortWindowExhausted is returned when every block in the civm port window
// is already assigned to another slot.
var ErrPortWindowExhausted = errors.New("civm port window exhausted")

// tempSuffix is the on-dir temp file suffix used for the atomic write.
const tempSuffix = ".tmp"

// Allocation is one persisted slot -> base assignment.
type Allocation struct {
	Slot string `json:"slot"`
	Base int    `json:"base"`
}

// Options configures Allocate. Every side effect is injected so unit tests run
// without real syscalls, flock, exec or filesystem access.
type Options struct {
	// StatePath is the JSON slot->base map (default DefaultPortBlockStatePath).
	StatePath string
	// BlockStart is the lowest base in the window (default
	// civm.DefaultRunnerPortBlockStart).
	BlockStart int
	// BlockSize is the number of ports per block / the base step (default
	// civm.DefaultRunnerPortBlockSize).
	BlockSize int
	// WindowEnd is the exclusive upper bound of the window (default
	// civm.DefaultRunnerPortWindowEnd).
	WindowEnd int

	ReadFileFn  func(path string) ([]byte, error)
	WriteFileFn func(path string, data []byte, perm os.FileMode) error
	MkdirAllFn  func(path string, perm os.FileMode) error
	// FlockFn locks the StatePath descriptor. The real implementation calls
	// syscall.Flock; tests inject a no-op.
	FlockFn func(fd int, how int) error
}

// DefaultOptions returns production wiring.
func DefaultOptions() Options {
	return Options{
		StatePath:   civm.DefaultPortBlockStatePath,
		BlockStart:  civm.DefaultRunnerPortBlockStart,
		BlockSize:   civm.DefaultRunnerPortBlockSize,
		WindowEnd:   civm.DefaultRunnerPortWindowEnd,
		ReadFileFn:  os.ReadFile,
		WriteFileFn: os.WriteFile,
		MkdirAllFn:  os.MkdirAll,
		FlockFn:     syscall.Flock,
	}
}

func applyDefaults(opts *Options) {
	if opts.StatePath == "" {
		opts.StatePath = civm.DefaultPortBlockStatePath
	}
	if opts.BlockStart == 0 {
		opts.BlockStart = civm.DefaultRunnerPortBlockStart
	}
	if opts.BlockSize == 0 {
		opts.BlockSize = civm.DefaultRunnerPortBlockSize
	}
	if opts.WindowEnd == 0 {
		opts.WindowEnd = civm.DefaultRunnerPortWindowEnd
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = os.ReadFile
	}
	if opts.WriteFileFn == nil {
		opts.WriteFileFn = os.WriteFile
	}
	if opts.MkdirAllFn == nil {
		opts.MkdirAllFn = os.MkdirAll
	}
	if opts.FlockFn == nil {
		opts.FlockFn = syscall.Flock
	}
}

// Allocate returns the sticky base port for slot, allocating the lowest free
// block when the slot has none yet. It is safe to call concurrently across
// processes: an exclusive flock on the StatePath descriptor serializes the
// whole read -> find -> write cycle and the new state is persisted atomically
// via temp file + os.Rename. ErrPortWindowExhausted is returned when no block
// is free.
func Allocate(opts Options, slot string) (int, error) {
	applyDefaults(&opts)
	if err := validateOptions(opts); err != nil {
		return 0, err
	}
	if strings.TrimSpace(slot) == "" {
		return 0, errors.New("portblock: slot obrigatorio")
	}

	if err := opts.MkdirAllFn(filepath.Dir(opts.StatePath), 0o755); err != nil {
		return 0, fmt.Errorf("portblock: criar dir de state %s: %w", opts.StatePath, err)
	}

	// Lock a STABLE sidecar, never StatePath itself: writeState persists via
	// temp file + os.Rename, which swaps StatePath's inode. flock binds to the
	// open inode, so locking StatePath would stop serializing the instant the
	// first writer renames, letting concurrent allocations race and hand the
	// same base to two slots. The .lock sidecar is created once and never
	// renamed, so its inode is stable and the flock is real cross-process
	// exclusion for the whole read -> find -> write cycle.
	lockPath := opts.StatePath + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, fmt.Errorf("portblock: abrir lock %s: %w", lockPath, err)
	}
	defer func() { _ = lf.Close() }()

	if err := opts.FlockFn(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return 0, fmt.Errorf("portblock: flock %s: %w", lockPath, err)
	}
	defer func() { _ = opts.FlockFn(int(lf.Fd()), syscall.LOCK_UN) }()

	state, err := readState(opts)
	if err != nil {
		return 0, err
	}

	if base, ok := state[slot]; ok {
		return base, nil
	}

	base, err := nextFreeBase(opts, state)
	if err != nil {
		return 0, err
	}
	state[slot] = base

	if err := writeState(opts, state); err != nil {
		return 0, err
	}
	return base, nil
}

func validateOptions(opts Options) error {
	if opts.BlockSize <= 0 {
		return fmt.Errorf("portblock: block-size deve ser >0, got %d", opts.BlockSize)
	}
	if opts.WindowEnd <= opts.BlockStart {
		return fmt.Errorf("portblock: window-end (%d) deve ser > block-start (%d)", opts.WindowEnd, opts.BlockStart)
	}
	return nil
}

// nextFreeBase returns the lowest base in [BlockStart, WindowEnd) stepping by
// BlockSize that is not already assigned to another slot.
func nextFreeBase(opts Options, state map[string]int) (int, error) {
	used := make(map[int]struct{}, len(state))
	for _, base := range state {
		used[base] = struct{}{}
	}
	for base := opts.BlockStart; base+opts.BlockSize <= opts.WindowEnd; base += opts.BlockSize {
		if _, taken := used[base]; !taken {
			return base, nil
		}
	}
	return 0, ErrPortWindowExhausted
}

// readState reads the persisted Allocation array and folds it into a
// slot->base map. A missing or empty file is an empty map, not an error.
func readState(opts Options) (map[string]int, error) {
	data, err := opts.ReadFileFn(opts.StatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int{}, nil
		}
		return nil, fmt.Errorf("portblock: ler state %s: %w", opts.StatePath, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]int{}, nil
	}
	var allocations []Allocation
	if err := json.Unmarshal(data, &allocations); err != nil {
		return nil, fmt.Errorf("portblock: parse state %s: %w", opts.StatePath, err)
	}
	state := make(map[string]int, len(allocations))
	for _, a := range allocations {
		state[a.Slot] = a.Base
	}
	return state, nil
}

// writeState persists the slot->base map atomically: it marshals deterministic,
// indented JSON to a temp file in the same directory and renames it over
// StatePath so a concurrent reader never observes a torn file.
func writeState(opts Options, state map[string]int) error {
	data, err := marshalState(state)
	if err != nil {
		return err
	}
	tmp := opts.StatePath + tempSuffix
	if err := opts.WriteFileFn(tmp, data, 0o600); err != nil {
		return fmt.Errorf("portblock: gravar temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, opts.StatePath); err != nil {
		return fmt.Errorf("portblock: rename %s -> %s: %w", tmp, opts.StatePath, err)
	}
	return nil
}

// marshalState renders the map with slot keys sorted so the on-disk JSON is
// deterministic regardless of Go map iteration order.
func marshalState(state map[string]int) ([]byte, error) {
	slots := make([]string, 0, len(state))
	for slot := range state {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	allocations := make([]Allocation, 0, len(slots))
	for _, slot := range slots {
		allocations = append(allocations, Allocation{Slot: slot, Base: state[slot]})
	}
	data, err := json.MarshalIndent(allocations, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("portblock: serializar state: %w", err)
	}
	return data, nil
}
