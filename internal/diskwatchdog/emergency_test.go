package diskwatchdog

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/cleanup"
	"github.com/emersonbusson/civm/internal/safedelete"
)

// TestBuildCleanupOptionsEmergencyWiring proves the watchdog arms the
// busy-bypass exactly at the emergency level (2026-06-10: 83% used, everything
// deferred-by-host-busy, guest ran to 0% free).
func TestBuildCleanupOptionsEmergencyWiring(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()

	if got := buildCleanupOptions(opts, civm.DefaultEmergencyBypassPct-1); got.EmergencyBypassIdle {
		t.Fatalf("below emergency level the busy deferral must stand")
	}
	if got := buildCleanupOptions(opts, civm.DefaultEmergencyBypassPct); !got.EmergencyBypassIdle {
		t.Fatalf("at emergency level the safe reclaim must bypass the busy deferral")
	}
	if got := buildCleanupOptions(opts, 99); !got.EmergencyBypassIdle {
		t.Fatalf("above emergency level the safe reclaim must bypass the busy deferral")
	}
	// Zero-value EmergencyPct (callers building Options by hand) falls back to
	// the default instead of arming at 0%.
	zero := Options{}
	if got := buildCleanupOptions(zero, civm.DefaultEmergencyBypassPct-1); got.EmergencyBypassIdle {
		t.Fatalf("zero EmergencyPct must fall back to the default, not arm at 0%%")
	}
}

// TestCheck_ExecuteBusyEmergencyRunsSafeReclaim proves the end-to-end wiring:
// at emergency disk usage with a busy host, Check's triggered cleanup runs the
// SAFE reclaim instead of deferring everything. Fully hermetic via the
// passthrough fakes — the emergency path deletes age-gated files, which a test
// must never aim at the real filesystem.
func TestCheck_ExecuteBusyEmergencyRunsSafeReclaim(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 * (1 << 30), 5 * (1 << 30), nil // 95% used — emergency
	}
	o.ThresholdPct = 60
	o.Execute = true
	o.ActivityFn = func(context.Context) ([]cleanup.Activity, error) {
		return []cleanup.Activity{{PID: 4321, Command: "/home/emdev/actions-runner/bin/Runner.Worker run"}}, nil
	}
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	o.WalkFn = func(string, fs.WalkDirFunc) error { return nil }
	o.GlobFn = func(string) ([]string, error) { return nil, nil }
	o.RemoveAllFn = func(string) error { return nil }
	o.SafeDeleteFn = func(context.Context, string) safedelete.Result { return safedelete.Result{} }

	r := Check(context.Background(), o)

	if r.Decision != DecisionCleanupTriggered {
		t.Fatalf("Decision = %v, want CleanupTriggered; err=%v", r.Decision, r.Err)
	}
	names := make([]string, 0, len(r.CleanupActions))
	for _, a := range r.CleanupActions {
		names = append(names, a.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "emergency-bypass-idle") {
		t.Fatalf("emergency usage while busy must arm the bypass; actions=%s", joined)
	}
	if strings.Contains(joined, "deferred-by-host-busy") {
		t.Fatalf("emergency must replace the busy deferral; actions=%s", joined)
	}
}
