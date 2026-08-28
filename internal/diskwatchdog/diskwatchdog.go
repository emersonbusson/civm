// Package diskwatchdog implements a disk-pressure watchdog: when the
// filesystem usage exceeds a threshold, it triggers aggressive cleanup
// (delegating to internal/cleanup). Stdlib-only.
package diskwatchdog

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"syscall"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/cleanup"
	"github.com/emersonbusson/civm/internal/safedelete"
)

// Decision is the watchdog outcome.
type Decision int

const (
	DecisionOK               Decision = iota // disk OK, nothing done
	DecisionCleanupTriggered                 // disk above threshold, cleanup ran
	DecisionError                            // erro lendo disk OR rodando cleanup
)

func (d Decision) String() string {
	switch d {
	case DecisionOK:
		return "ok"
	case DecisionCleanupTriggered:
		return "cleanup-triggered"
	case DecisionError:
		return "error"
	}
	return "?"
}

// Result captures watchdog execution outcome.
type Result struct {
	Decision       Decision
	Path           string
	UsedPct        int
	UsedGB         int64
	TotalGB        int64
	ThresholdPct   int
	CleanupActions []cleanup.Action // populated when DecisionCleanupTriggered
	Err            error
}

// Options control the watchdog.
type Options struct {
	Path         string // diretorio a monitorar (ex: "/")
	ThresholdPct int    // disparar cleanup se usedPct > ThresholdPct (default 70)
	// EmergencyPct: at/above this usage the triggered cleanup stops deferring
	// the SAFE reclaim by host-busy (sets cleanup.EmergencyBypassIdle).
	// Default civm.DefaultEmergencyBypassPct.
	EmergencyPct int
	Execute      bool   // false = dry-run (apenas verifica + reporta)
	WorkDir      string // passado para cleanup.Options.WorkDir
	TmpDir       string // passado para cleanup.Options.TmpDir
	StatfsFn     func(path string) (totalBytes, freeBytes uint64, err error)
	RunFn        func(ctx context.Context, name string, args ...string) ([]byte, error)
	ActivityFn   func(ctx context.Context) ([]cleanup.Activity, error)
	// Optional passthroughs to cleanup.Options so tests stay hermetic — the
	// emergency bypass deletes age-gated files even while busy, which a test
	// must never do against the real filesystem. nil keeps production defaults.
	WalkFn       func(root string, fn fs.WalkDirFunc) error
	GlobFn       func(pattern string) ([]string, error)
	RemoveAllFn  func(path string) error
	SafeDeleteFn func(ctx context.Context, path string) safedelete.Result
}

// DefaultOptions returns sane production defaults.
func DefaultOptions() Options {
	return Options{
		Path:         "/",
		ThresholdPct: civm.DefaultWatchdogThresholdPct,
		EmergencyPct: civm.DefaultEmergencyBypassPct,
		Execute:      false,
		WorkDir:      civm.DefaultWorkDir,
		TmpDir:       civm.DefaultTmpDir,
		StatfsFn:     defaultStatfs,
		RunFn:        defaultRun,
	}
}

// buildCleanupOptions maps the watchdog options to a triggered cleanup run.
// Extracted so the emergency wiring is unit-testable without executing a real
// cleanup (which would touch live caches).
func buildCleanupOptions(opts Options, usedPct int) cleanup.Options {
	cleanOpts := cleanup.DefaultOptions()
	cleanOpts.Execute = opts.Execute
	cleanOpts.WorkDir = opts.WorkDir
	cleanOpts.TmpDir = opts.TmpDir
	// Aggressive: shorter thresholds when disk pressure
	cleanOpts.TmpThreshold = 1 * time.Hour
	cleanOpts.WorkThreshold = 24 * time.Hour
	cleanOpts.RunFn = opts.RunFn
	cleanOpts.ActivityFn = opts.ActivityFn
	if opts.WalkFn != nil {
		cleanOpts.WalkFn = opts.WalkFn
	}
	if opts.GlobFn != nil {
		cleanOpts.GlobFn = opts.GlobFn
	}
	if opts.RemoveAllFn != nil {
		cleanOpts.RemoveAllFn = opts.RemoveAllFn
	}
	if opts.SafeDeleteFn != nil {
		cleanOpts.SafeDeleteFn = opts.SafeDeleteFn
	}
	if opts.EmergencyPct == 0 {
		opts.EmergencyPct = civm.DefaultEmergencyBypassPct
	}
	// At emergency usage the busy-host deferral is what let the disk run to 0
	// in the 2026-06-10 wedge: the safe reclaim must run even mid-job.
	cleanOpts.EmergencyBypassIdle = usedPct >= opts.EmergencyPct
	return cleanOpts
}

// Check reads disk usage and triggers cleanup if above threshold.
func Check(ctx context.Context, opts Options) Result {
	if opts.StatfsFn == nil {
		opts.StatfsFn = defaultStatfs
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.Path == "" {
		opts.Path = "/"
	}
	if opts.ThresholdPct == 0 {
		opts.ThresholdPct = civm.DefaultWatchdogThresholdPct
	}
	if opts.ThresholdPct < 1 || opts.ThresholdPct > 99 {
		return Result{
			Decision:     DecisionError,
			Path:         opts.Path,
			ThresholdPct: opts.ThresholdPct,
			Err:          fmt.Errorf("threshold-pct deve ficar entre 1 e 99, got %d", opts.ThresholdPct),
		}
	}
	r := Result{Path: opts.Path, ThresholdPct: opts.ThresholdPct}
	total, free, err := opts.StatfsFn(opts.Path)
	if err != nil {
		r.Decision = DecisionError
		r.Err = err
		return r
	}
	if total == 0 {
		r.Decision = DecisionError
		r.Err = fmt.Errorf("statfs total = 0")
		return r
	}
	used := total - free
	r.UsedGB = int64(used / (1 << 30))
	r.TotalGB = int64(total / (1 << 30))
	r.UsedPct = int(used * 100 / total)
	if r.UsedPct <= opts.ThresholdPct {
		r.Decision = DecisionOK
		return r
	}
	// Above threshold: trigger cleanup
	cleanOpts := buildCleanupOptions(opts, r.UsedPct)
	r.CleanupActions = cleanup.Run(ctx, cleanOpts)
	r.Decision = DecisionCleanupTriggered
	for _, a := range r.CleanupActions {
		if a.Err != nil {
			r.Decision = DecisionError
			r.Err = a.Err
			break
		}
	}
	return r
}

// ExitCode returns exit code matching Decision.
func (r Result) ExitCode() int {
	switch r.Decision {
	case DecisionOK:
		return 0
	case DecisionCleanupTriggered:
		return 0 // cleanup successful is OK
	case DecisionError:
		return 2
	}
	return 1
}

// Render writes a human-readable report.
func (r Result) Render(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Disk watchdog: %s\n", r.Path)
	_, _ = fmt.Fprintf(w, "Used: %d GB / %d GB (%d%%) | Threshold: %d%%\n",
		r.UsedGB, r.TotalGB, r.UsedPct, r.ThresholdPct)
	_, _ = fmt.Fprintf(w, "Decision: %s\n", r.Decision)
	if r.Err != nil {
		_, _ = fmt.Fprintf(w, "Error: %v\n", r.Err)
	}
	if len(r.CleanupActions) > 0 {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "Aggressive cleanup actions (TmpThreshold=1h, WorkThreshold=24h):")
		for _, a := range r.CleanupActions {
			status := "(dry-run)"
			if a.Err != nil {
				status = "erro: " + a.Err.Error()
			} else if cleanup.IsDeferral(a.Name) {
				status = "deferido"
			} else if a.Executed {
				status = "aplicado"
			}
			_, _ = fmt.Fprintf(w, "  %-14s found=%s freed=%s %s\n",
				a.Name, cleanup.FormatBytes(a.BytesFound), cleanup.FormatBytes(a.BytesFreed), status)
		}
	}
}

// ---- defaults ----

func defaultStatfs(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total := uint64(st.Blocks) * uint64(st.Bsize)
	free := uint64(st.Bavail) * uint64(st.Bsize)
	return total, free, nil
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
