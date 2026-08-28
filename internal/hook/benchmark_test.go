package hook

import (
	"context"
	"os"
	"testing"
)

// BenchmarkSafeWorkRoot establishes a baseline for the path traversal guard
// called by each workRoots add(). This is sensitive because it is pure string
// work in a hot path.
func BenchmarkSafeWorkRoot(b *testing.B) {
	valid := "/home/emdev/actions-runner-acme/_work"
	invalid := "../home/actions-runner/_work"
	b.ResetTimer()
	for range b.N {
		_ = safeWorkRoot(valid)
		_ = safeWorkRoot(invalid)
	}
}

// BenchmarkCleanup_RoutineNoPressure captures the job-completed path
// (purgeCaches=false): cleanWorkRoot plus four commandActions, all with
// no-op mocks. This catches append/allocation regressions.
func BenchmarkCleanup_RoutineNoPressure(b *testing.B) {
	opts := Options{
		Execute:     true,
		WorkRoot:    "/home/civm-test/actions-runner/_work",
		RemoveAllFn: func(string) error { return nil },
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		ReadDirFn:   func(string) ([]os.DirEntry, error) { return nil, nil },
		RunFn:       func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		StatfsFn:    func(string) (uint64, uint64, error) { return 100, 60, nil },
	}
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		_ = cleanup(opts, ctx, false)
	}
}

// BenchmarkCleanup_DiskPressure captures job-started with purgeCaches=true:
// cleanWorkRoot plus four cachePaths plus four commandActions. The delta vs.
// routine mode estimates cache wipe cost.
func BenchmarkCleanup_DiskPressure(b *testing.B) {
	b.Setenv("HOME", "/home/civm-bench")
	opts := Options{
		Execute:     true,
		WorkRoot:    "/home/civm-test/actions-runner/_work",
		RemoveAllFn: func(string) error { return nil },
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		ReadDirFn:   func(string) ([]os.DirEntry, error) { return nil, nil },
		RunFn:       func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		StatfsFn:    func(string) (uint64, uint64, error) { return 100, 5, nil },
	}
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		_ = cleanup(opts, ctx, true)
	}
}

// BenchmarkAppendLog measures slog emission overhead for hook events. It runs
// once per hook invocation; the baseline helps catch regressions if this grows
// to multiple handlers such as journald.
func BenchmarkAppendLog(b *testing.B) {
	dir := b.TempDir()
	opts := Options{
		Execute:    true,
		LogPath:    dir + "/hooks.jsonl",
		MkdirAllFn: os.MkdirAll,
	}
	res := Result{
		Event: EventJobCompleted, Decision: DecisionCleanupApplied,
		Repository: "acme/civm", RunID: "12345",
		DiskUsedPct: 42,
		Actions: []Action{
			{Name: "work_root", Path: "/home/x/actions-runner/_work", Executed: true},
			{Name: "docker_prune", Executed: true},
		},
	}
	b.ResetTimer()
	for range b.N {
		_ = appendLog(opts, res)
	}
}
