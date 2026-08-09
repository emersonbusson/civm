package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/advoq/civm/internal/hook"
	"github.com/advoq/civm/internal/idle"
)

var testWatchdogNow = time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

func baseWatchdogOptions(t *testing.T) WatchdogOptions {
	t.Helper()
	opts := DefaultWatchdogOptions()
	opts.Execute = true
	opts.Repos = []string{"acme/civm"}
	opts.InferRepos = false
	opts.NetworkFn = func(context.Context, time.Duration) error { return nil }
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.acme-civm.civm-self.service",
			Repo:        "acme/civm",
			Name:        "civm-self",
			ActiveState: "active",
			SubState:    "running",
		}}, nil
	}
	opts.GitHubRunnersFn = func(context.Context, string) ([]WatchdogGitHubRunner, error) {
		return []WatchdogGitHubRunner{{
			Repo:   "acme/civm",
			Name:   "civm-self",
			Status: "online",
			Labels: []string{"self-hosted", "civm"},
		}}, nil
	}
	opts.HookInstallFn = func(context.Context, hook.InstallOptions) hook.InstallResult {
		return hook.InstallResult{Executed: true, HooksDir: hook.DefaultHooksDir}
	}
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return []byte("active\n"), nil }
	opts.SleepFn = func(time.Duration) {}
	opts.NowFn = func() time.Time { return testWatchdogNow }
	opts.MarkerPath = t.TempDir() + "/reruns.json"
	opts.ReadFileFn = os.ReadFile
	opts.WriteFileFn = os.WriteFile
	opts.MkdirAllFn = os.MkdirAll
	return opts
}

// A maintenance drain deliberately stops the listener while queued work may
// still exist. The watchdog must not reinterpret that stopped unit as broken
// and restart it behind the operator's back.
func TestWatchdogMaintenanceStateSkipsEveryMutation(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.MaintenanceStatePath = t.TempDir() + "/maintenance.json"
	if err := os.WriteFile(opts.MaintenanceStatePath, []byte(`{"drained_at":"2026-08-02T08:44:44Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	networkCalls := 0
	statusCalls := 0
	hookCalls := 0
	restartCalls := 0
	opts.NetworkFn = func(context.Context, time.Duration) error {
		networkCalls++
		return nil
	}
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		statusCalls++
		return nil, nil
	}
	opts.HookInstallFn = func(context.Context, hook.InstallOptions) hook.InstallResult {
		hookCalls++
		return hook.InstallResult{}
	}
	opts.RestartFn = func(context.Context, string) error {
		restartCalls++
		return nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0; events=%+v", report.Exit, report.Events)
	}
	if !hasWatchdogEventWithReason(report, "runner-restart-skipped", "maintenance-active") {
		t.Fatalf("events = %+v, want runner-restart-skipped maintenance-active", report.Events)
	}
	if networkCalls != 0 || statusCalls != 0 || hookCalls != 0 || restartCalls != 0 {
		t.Fatalf("maintenance mutated/probed remote state: network=%d status=%d hooks=%d restarts=%d",
			networkCalls, statusCalls, hookCalls, restartCalls)
	}
}

func TestWatchdogMaintenanceStateUnknownFailsClosed(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.MaintenanceActiveFn = func(string) (bool, error) {
		return false, errors.New("permission denied")
	}
	restartCalls := 0
	opts.RestartFn = func(context.Context, string) error {
		restartCalls++
		return nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 1 {
		t.Fatalf("Exit = %d, want 1; events=%+v", report.Exit, report.Events)
	}
	if !hasWatchdogEventWithReason(report, "runner-restart-skipped", "maintenance-state-unknown") {
		t.Fatalf("events = %+v, want maintenance-state-unknown", report.Events)
	}
	if restartCalls != 0 {
		t.Fatalf("restart calls = %d, want 0", restartCalls)
	}
}

func TestWatchdogNetworkDownDoesNotMutate(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.NetworkFn = func(context.Context, time.Duration) error { return errors.New("dial timeout") }
	hookCalls := 0
	restartCalls := 0
	rerunCalls := 0
	opts.HookInstallFn = func(context.Context, hook.InstallOptions) hook.InstallResult {
		hookCalls++
		return hook.InstallResult{}
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "sudo" && strings.Join(args, " ") == "systemctl restart actions.runner.acme-civm.civm-self.service" {
			restartCalls++
		}
		return []byte("active\n"), nil
	}
	opts.RerunNetworkFailures = true
	opts.RerunFn = func(context.Context, string, int64) error {
		rerunCalls++
		return nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 1 {
		t.Fatalf("Exit = %d, want 1", report.Exit)
	}
	if !hasWatchdogEvent(report, "network-down") {
		t.Fatalf("events = %+v, want network-down", report.Events)
	}
	if hookCalls != 0 || restartCalls != 0 || rerunCalls != 0 {
		t.Fatalf("mutated despite network down: hooks=%d restarts=%d reruns=%d", hookCalls, restartCalls, rerunCalls)
	}
}

func TestWatchdogBusyHostDoesNotMutate(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RerunNetworkFailures = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.acme-civm.civm-self.service",
			Repo:        "acme/civm",
			Name:        "civm-self",
			ActiveState: "failed",
			SubState:    "failed",
		}}, nil
	}
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return []idle.Activity{{PID: 99, Command: "Runner.Worker run"}}, nil
	}
	hookCalls := 0
	restartCalls := 0
	rerunCalls := 0
	opts.HookInstallFn = func(context.Context, hook.InstallOptions) hook.InstallResult {
		hookCalls++
		return hook.InstallResult{}
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "sudo" && strings.Contains(strings.Join(args, " "), "systemctl restart") {
			restartCalls++
		}
		return []byte("active\n"), nil
	}
	opts.ListRunsFn = func(context.Context, string, int) ([]WatchdogRun, error) {
		return []WatchdogRun{{ID: 1, HeadSHA: "abc", Conclusion: "failure", PullRequests: []WatchdogPullRequestRef{{Number: 7}}}}, nil
	}
	opts.PullRequestFn = func(context.Context, string, int) (WatchdogPullRequest, error) {
		return WatchdogPullRequest{Number: 7, State: "open", MergeableState: "clean"}, nil
	}
	logCalls := 0
	opts.RunLogFn = func(context.Context, string, int64) (string, error) {
		logCalls++
		return "Run actions/checkout@v5\nfatal: early EOF\ninvalid index-pack output", nil
	}
	opts.RerunFn = func(context.Context, string, int64) error {
		rerunCalls++
		return nil
	}

	report := Watchdog(context.Background(), opts)
	// host-busy is the expected steady state on a shared runner box: the
	// watchdog must defer maintenance (no mutations) AND report success
	// (exit 0). Marking the systemd unit failed on every busy tick would
	// keep it perpetually red and mask genuine faults. (Kahneman #13: the
	// prior assertion wanted exit 1, locking in the opposite of the purpose.)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (host-busy deferral is success) events=%+v", report.Exit, report.Events)
	}
	if !hasWatchdogEventWithReason(report, "rerun-skipped", "host-busy") {
		t.Fatalf("events = %+v, want rerun-skipped host-busy", report.Events)
	}
	if hookCalls != 0 || restartCalls != 0 || rerunCalls != 0 {
		t.Fatalf("mutated despite busy host: hooks=%d restarts=%d reruns=%d", hookCalls, restartCalls, rerunCalls)
	}
}

func TestWatchdogIdleUnknownDefersWithoutFailing(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RerunNetworkFailures = true
	// A failed idle probe means we cannot prove the host is idle, so the
	// watchdog must refrain from acting. Like host-busy, this is a safe
	// deferral, not a watchdog failure: exit 0 with a warning event, never a
	// red systemd unit. (SPEC RF-6: non-zero is reserved for real faults.)
	hookCalls := 0
	restartCalls := 0
	rerunCalls := 0
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return nil, errors.New("ps probe failed")
	}
	opts.HookInstallFn = func(context.Context, hook.InstallOptions) hook.InstallResult {
		hookCalls++
		return hook.InstallResult{}
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "sudo" && strings.Contains(strings.Join(args, " "), "systemctl restart") {
			restartCalls++
		}
		return []byte("active\n"), nil
	}
	opts.RerunFn = func(context.Context, string, int64) error {
		rerunCalls++
		return nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (host-idle-unknown deferral is success) events=%+v", report.Exit, report.Events)
	}
	if !hasWatchdogEventWithReason(report, "runner-restart-skipped", "host-idle-unknown") {
		t.Fatalf("events = %+v, want runner-restart-skipped host-idle-unknown", report.Events)
	}
	if hookCalls != 0 || restartCalls != 0 || rerunCalls != 0 {
		t.Fatalf("mutated despite unknown host state: hooks=%d restarts=%d reruns=%d", hookCalls, restartCalls, rerunCalls)
	}
}

// --- ITEM-10 broken-runner auto-restart (DT-8) ---

type detectIO struct {
	files      map[string][]byte
	restarted  []string
	restartErr error
}

func newDetectIO() *detectIO { return &detectIO{files: map[string][]byte{}} }

func (d *detectIO) opts(now time.Time) WatchdogOptions {
	return WatchdogOptions{
		Execute:            true,
		HooksLogPath:       "/var/log/civm/hooks.jsonl",
		MarkerPath:         "/var/lib/civm/marker.json",
		AutoRestartPerHour: 3,
		NowFn:              func() time.Time { return now },
		ReadFileFn: func(p string) ([]byte, error) {
			if b, ok := d.files[p]; ok {
				return b, nil
			}
			return nil, os.ErrNotExist
		},
		WriteFileFn: func(p string, b []byte, _ os.FileMode) error { d.files[p] = b; return nil },
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		RestartFn: func(_ context.Context, unit string) error {
			d.restarted = append(d.restarted, unit)
			return d.restartErr
		},
	}
}

func sentinelLine(now time.Time, workRoot string) string {
	return `{"time":"` + now.Format(time.RFC3339) + `","event":"job-completed","decision":"error","work_root":"` +
		workRoot + `","actions":[{"name":"work_root","error":"wrapper rm failed: boom"}]}`
}

func brokenRunnerSystemd() []Status {
	return []Status{
		{UnitName: "actions.runner.acme-org.civm-acme-org.service", Name: "civm-acme-org", WorkingDirectory: "/home/emdev/actions-runner-acme-org"},
		{UnitName: "actions.runner.other-peer.civm-peer.service", Name: "civm-peer", WorkingDirectory: "/home/emdev/actions-runner-peer"},
	}
}

func TestDetectBrokenRunnerRestartsCorrectUnit(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	io.files["/var/log/civm/hooks.jsonl"] = []byte(sentinelLine(now.Add(-5*time.Minute), "/home/emdev/actions-runner-acme-org/_work") + "\n")
	var report WatchdogReport
	detectBrokenRunner(context.Background(), io.opts(now), brokenRunnerSystemd(), &report)
	if len(io.restarted) != 1 || io.restarted[0] != "actions.runner.acme-org.civm-acme-org.service" {
		t.Fatalf("restarted = %v, want only the acme-org unit (deterministic WorkRoot map)", io.restarted)
	}
	if !hasWatchdogEvent(report, "runner-auto-restarted") {
		t.Fatalf("events = %+v, want runner-auto-restarted", report.Events)
	}
}

func TestDetectBrokenRunnerNoSentinelNoRestart(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	// job-completed with a CLEAN work_root action (no error) — not a sentinel.
	io.files["/var/log/civm/hooks.jsonl"] = []byte(`{"time":"` + now.Format(time.RFC3339) + `","decision":"ok","work_root":"/home/emdev/actions-runner-acme-org/_work","actions":[{"name":"work_root","executed":true}]}` + "\n")
	var report WatchdogReport
	detectBrokenRunner(context.Background(), io.opts(now), brokenRunnerSystemd(), &report)
	if len(io.restarted) != 0 {
		t.Fatalf("restarted = %v, want none (no broken sentinel)", io.restarted)
	}
}

func TestDetectBrokenRunnerStaleSentinelIgnored(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	io.files["/var/log/civm/hooks.jsonl"] = []byte(sentinelLine(now.Add(-3*time.Hour), "/home/emdev/actions-runner-acme-org/_work") + "\n")
	var report WatchdogReport
	detectBrokenRunner(context.Background(), io.opts(now), brokenRunnerSystemd(), &report)
	if len(io.restarted) != 0 {
		t.Fatalf("restarted = %v, want none (sentinel older than 1h)", io.restarted)
	}
}

func TestDetectBrokenRunnerUnknownWorkRootDoesNotTouchWrongUnit(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	// work_root maps to NO known unit → must restart nothing (never guess).
	io.files["/var/log/civm/hooks.jsonl"] = []byte(sentinelLine(now, "/home/emdev/actions-runner-ghost/_work") + "\n")
	var report WatchdogReport
	detectBrokenRunner(context.Background(), io.opts(now), brokenRunnerSystemd(), &report)
	if len(io.restarted) != 0 {
		t.Fatalf("restarted = %v, want none (no unit owns the work_root)", io.restarted)
	}
	if !hasWatchdogEventWithReason(report, "runner-auto-restart-skipped", "no-unit-for-work-root") {
		t.Fatalf("events = %+v, want no-unit-for-work-root skip", report.Events)
	}
}

func TestDetectBrokenRunnerRateCapSkips(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	io.files["/var/log/civm/hooks.jsonl"] = []byte(sentinelLine(now, "/home/emdev/actions-runner-acme-org/_work") + "\n")
	// Pre-seed the marker at the cap for this unit in the current hour.
	io.files["/var/lib/civm/marker.json"] = []byte(`{"reruns":{},"auto_restarts":{"actions.runner.acme-org.civm-acme-org.service":{"count":3,"window_start":"` + now.Format(time.RFC3339) + `"}}}`)
	var report WatchdogReport
	detectBrokenRunner(context.Background(), io.opts(now), brokenRunnerSystemd(), &report)
	if len(io.restarted) != 0 {
		t.Fatalf("restarted = %v, want none (rate cap reached)", io.restarted)
	}
	if !hasWatchdogEventWithReason(report, "runner-auto-restart-skipped", "rate-cap-reached") {
		t.Fatalf("events = %+v, want rate-cap-reached skip", report.Events)
	}
}

func TestDetectBrokenRunnerRestartErrorExits2(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	io.restartErr = errors.New("systemctl restart failed")
	io.files["/var/log/civm/hooks.jsonl"] = []byte(sentinelLine(now, "/home/emdev/actions-runner-acme-org/_work") + "\n")
	var report WatchdogReport
	detectBrokenRunner(context.Background(), io.opts(now), brokenRunnerSystemd(), &report)
	if report.Exit != 2 {
		t.Fatalf("Exit = %d, want 2 (real restart failure)", report.Exit)
	}
}

func TestDetectBrokenRunnerDryRunCandidateOnly(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	opts := io.opts(now)
	opts.Execute = false
	io.files["/var/log/civm/hooks.jsonl"] = []byte(sentinelLine(now, "/home/emdev/actions-runner-acme-org/_work") + "\n")
	var report WatchdogReport
	detectBrokenRunner(context.Background(), opts, brokenRunnerSystemd(), &report)
	if len(io.restarted) != 0 {
		t.Fatalf("restarted = %v, want none in dry-run", io.restarted)
	}
	if !hasWatchdogEvent(report, "runner-auto-restarted") {
		t.Fatalf("dry-run should surface the candidate event: %+v", report.Events)
	}
}

func TestUnitForWorkRootRejectsPrefixCollision(t *testing.T) {
	t.Parallel()
	systemd := []Status{
		{UnitName: "actions.runner.bare.civm-bare.service", WorkingDirectory: "/home/emdev/actions-runner"},
		{UnitName: "actions.runner.acme.civm-app.service", WorkingDirectory: "/home/emdev/actions-runner-acme"},
	}
	if got := unitForWorkRoot("/home/emdev/actions-runner-acme/_work", systemd); got != "actions.runner.acme.civm-app.service" {
		t.Fatalf("got %q, want the -acme unit (no prefix collision with bare actions-runner)", got)
	}
	if got := unitForWorkRoot("/home/emdev/actions-runner/_work", systemd); got != "actions.runner.bare.civm-bare.service" {
		t.Fatalf("got %q, want the bare unit", got)
	}
	if got := unitForWorkRoot("/home/emdev/actions-runner-ghost/_work/", systemd); got != "" {
		t.Fatalf("got %q, want '' for an unowned work_root", got)
	}
}

func TestDetectBrokenRunnerMarkerWriteFailureFailsClosed(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	io.files["/var/log/civm/hooks.jsonl"] = []byte(sentinelLine(now, "/home/emdev/actions-runner-acme-org/_work") + "\n")
	opts := io.opts(now)
	// Persistent marker-write failure (correlated with the disk fault that wedges
	// the runner). The cap must hold: NO restart without persisting the slot.
	opts.WriteFileFn = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	for i := 0; i < 6; i++ {
		var report WatchdogReport
		detectBrokenRunner(context.Background(), opts, brokenRunnerSystemd(), &report)
		if report.Exit != 2 {
			t.Fatalf("tick %d: Exit = %d, want 2 (marker-write-failed fails closed)", i, report.Exit)
		}
	}
	if len(io.restarted) != 0 {
		t.Fatalf("restarted %d time(s) with unwritable cap state; want 0 (fail-closed)", len(io.restarted))
	}
}

func TestDetectBrokenRunnerDedupesSameSentinelAcrossTicks(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	io := newDetectIO()
	io.files["/var/log/civm/hooks.jsonl"] = []byte(sentinelLine(now.Add(-2*time.Minute), "/home/emdev/actions-runner-acme-org/_work") + "\n")
	opts := io.opts(now)
	// The same sentinel line persists in the log across ticks; it must restart
	// the runner exactly ONCE (dedup), not once per tick up to the cap.
	for i := 0; i < 4; i++ {
		var report WatchdogReport
		detectBrokenRunner(context.Background(), opts, brokenRunnerSystemd(), &report)
	}
	if len(io.restarted) != 1 {
		t.Fatalf("restarted %d times for one persistent sentinel; want exactly 1 (dedup)", len(io.restarted))
	}
}

func TestWatchdogRepairsHooksWhenIdle(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	hookCalls := 0
	opts.HookInstallFn = func(_ context.Context, got hook.InstallOptions) hook.InstallResult {
		hookCalls++
		if !got.Execute {
			t.Fatalf("hook install Execute = false, want true")
		}
		if got.RestartRunners {
			t.Fatalf("watchdog hook repair must not restart all runners directly")
		}
		return hook.InstallResult{Executed: true, HooksDir: got.HooksDir, RunnerEnvFiles: []string{"/home/runner/actions-runner/.env"}}
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 events=%+v", report.Exit, report.Events)
	}
	if hookCalls != 1 {
		t.Fatalf("hookCalls = %d, want 1", hookCalls)
	}
	if !hasWatchdogEvent(report, "hooks-repaired") {
		t.Fatalf("events = %+v, want hooks-repaired", report.Events)
	}
}

func TestWatchdogRestartsFailedSystemdRunner(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.acme-civm.civm-self.service",
			Repo:        "acme/civm",
			Name:        "civm-self",
			ActiveState: "failed",
			SubState:    "failed",
		}}, nil
	}
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if strings.HasPrefix(call, "systemctl is-active") {
			return []byte("active\n"), nil
		}
		return nil, nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 events=%+v", report.Exit, report.Events)
	}
	if !hasWatchdogEvent(report, "runner-restarted") {
		t.Fatalf("events = %+v, want runner-restarted", report.Events)
	}
	assertWatchdogCall(t, calls, "sudo systemctl restart actions.runner.acme-civm.civm-self.service")
}

func TestWatchdogRestartsFailedOrgRunnerWhenReposCannotBeInferred(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	opts.Repos = nil
	opts.InferRepos = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.advoq.civm-advoq-org.service",
			Repo:        "advoq",
			Name:        "civm-advoq-org",
			ActiveState: "failed",
			SubState:    "failed",
		}}, nil
	}
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value":
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			return []byte("active\n"), nil
		default:
			return nil, nil
		}
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		if !strings.HasSuffix(path, "/.runner") {
			return nil, errors.New("unexpected read: " + path)
		}
		return append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"gitHubUrl":"https://github.com/advoq"}`)...), nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 events=%+v", report.Exit, report.Events)
	}
	if !report.RunnerOnline {
		t.Fatalf("RunnerOnline = false, want true after local systemd repair")
	}
	assertWatchdogCall(t, calls, "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service")
	if !hasWatchdogEvent(report, "runner-restarted") {
		t.Fatalf("events = %+v, want runner-restarted", report.Events)
	}
	if !hasWatchdogEventWithReason(report, "rerun-skipped", "no-repos") {
		t.Fatalf("events = %+v, want no-repos note", report.Events)
	}
}

func TestWatchdogOrgBusyRestartHasOneHourCooldown(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	opts.IdleProbeDelay = 0
	opts.QueueStallDwell = 5 * time.Minute
	opts.NowFn = func() time.Time { return now }
	opts.Repos = nil
	opts.InferRepos = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName: "actions.runner.advoq.civm-advoq-org.service", Repo: "advoq",
			Name: "civm-advoq-org", ActiveState: "active", SubState: "running",
		}}, nil
	}
	restarts := 0
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value":
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		case "gh api /orgs/advoq/actions/runners":
			return []byte(`{"runners":[{"name":"civm-advoq-org","status":"online","busy":true}]}`), nil
		case "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service":
			restarts++
			return nil, nil
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			return []byte("active\n"), nil
		default:
			return nil, nil
		}
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		if strings.HasSuffix(path, "/.runner") {
			return []byte(`{"gitHubUrl":"https://github.com/advoq"}`), nil
		}
		return nil, os.ErrNotExist
	}

	first := Watchdog(context.Background(), opts)
	if first.Exit != 0 || restarts != 0 ||
		!hasWatchdogEventWithReason(first, "runner-restart-skipped", "org-busy-dwell") {
		t.Fatalf("first=%+v restarts=%d, want persistent dwell arm", first, restarts)
	}
	now = now.Add(6 * time.Minute)
	second := Watchdog(context.Background(), opts)
	third := Watchdog(context.Background(), opts)
	if second.Exit != 0 || third.Exit != 0 || restarts != 1 {
		t.Fatalf("second=%+v third=%+v restarts=%d, want one restart after dwell", second, third, restarts)
	}
	if !hasWatchdogEventWithReason(third, "runner-restart-skipped", "org-busy-cooldown") {
		t.Fatalf("third events=%+v, want cooldown", third.Events)
	}
}

func TestWatchdogOrgBusyDwellClearsWhenWorkerAppears(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	opts := baseWatchdogOptions(t)
	opts.IdleProbeDelay = 0
	opts.QueueStallDwell = 5 * time.Minute
	opts.NowFn = func() time.Time { return now }
	opts.Repos = nil
	opts.InferRepos = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName: "actions.runner.advoq.civm-advoq-org.service", Repo: "advoq",
			Name: "civm-advoq-org", ActiveState: "active", SubState: "running",
		}}, nil
	}
	workerActive := false
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		if workerActive {
			return []idle.Activity{{PID: 42, Command: "Runner.Worker run"}}, nil
		}
		return nil, nil
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value":
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		case "gh api /orgs/advoq/actions/runners":
			return []byte(`{"runners":[{"name":"civm-advoq-org","status":"online","busy":true}]}`), nil
		default:
			return nil, nil
		}
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		if strings.HasSuffix(path, "/.runner") {
			return []byte(`{"gitHubUrl":"https://github.com/advoq"}`), nil
		}
		return nil, os.ErrNotExist
	}
	restarts := 0
	opts.RestartFn = func(context.Context, string) error { restarts++; return nil }

	first := Watchdog(context.Background(), opts)
	workerActive = true
	now = now.Add(time.Minute)
	second := Watchdog(context.Background(), opts)
	workerActive = false
	now = now.Add(10 * time.Minute)
	third := Watchdog(context.Background(), opts)
	if restarts != 0 ||
		!hasWatchdogEventWithReason(first, "runner-restart-skipped", "org-busy-dwell") ||
		!hasWatchdogEventWithReason(third, "runner-restart-skipped", "org-busy-dwell") {
		t.Fatalf("dwell survived real Worker: restarts=%d first=%+v second=%+v third=%+v", restarts, first, second, third)
	}
}

func TestWatchdogSkipsNoRepoRestartWhenWorkerAppears(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	opts.Repos = nil
	opts.InferRepos = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.advoq.civm-advoq-org.service",
			Repo:        "advoq",
			Name:        "civm-advoq-org",
			ActiveState: "failed",
			SubState:    "failed",
		}}, nil
	}
	activityCalls := 0
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		activityCalls++
		if activityCalls >= 3 {
			return []idle.Activity{{PID: 99, Command: "Runner.Worker run"}}, nil
		}
		return nil, nil
	}
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value" {
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		}
		return nil, nil
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		if !strings.HasSuffix(path, "/.runner") {
			return nil, errors.New("unexpected read: " + path)
		}
		return []byte(`{"gitHubUrl":"https://github.com/advoq"}`), nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 events=%+v", report.Exit, report.Events)
	}
	if !hasWatchdogEventWithReason(report, "runner-restart-skipped", "host-busy") {
		t.Fatalf("events = %+v, want host-busy restart skip", report.Events)
	}
	for _, call := range calls {
		if strings.Contains(call, "systemctl restart") {
			t.Fatalf("watchdog restarted after worker appeared: %v", calls)
		}
	}
}

func TestWatchdogTreatsHealthyOrgRunnerWithoutRepoAsWarning(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.Repos = nil
	opts.InferRepos = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.advoq.civm-advoq-org.service",
			Repo:        "advoq",
			Name:        "civm-advoq-org",
			ActiveState: "active",
			SubState:    "running",
		}}, nil
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value" {
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		}
		return nil, nil
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		if !strings.HasSuffix(path, "/.runner") {
			return nil, errors.New("unexpected read: " + path)
		}
		return append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"gitHubUrl":"https://github.com/advoq"}`)...), nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 for healthy org runner warning events=%+v", report.Exit, report.Events)
	}
	if !report.RunnerOnline {
		t.Fatalf("RunnerOnline = false, want true for healthy local org runner")
	}
	if !hasWatchdogEventWithReason(report, "rerun-skipped", "no-repos") {
		t.Fatalf("events = %+v, want no-repos warning", report.Events)
	}
}

func TestWatchdogRestartsOnlineIdleOrgRunnerWithPersistentCivmQueueOnce(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	opts := baseWatchdogOptions(t)
	opts.Repos = nil
	opts.InferRepos = true
	opts.QueueStallDwell = 5 * time.Minute
	opts.IdleProbeDelay = 0
	opts.NowFn = func() time.Time { return now }
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.advoq.civm-advoq-org.service",
			Repo:        "advoq",
			Org:         "advoq",
			Name:        "civm-advoq-org",
			ActiveState: "active",
			SubState:    "running",
		}}, nil
	}
	opts.QueueReposFn = func() ([]string, error) {
		return []string{"advoq/advoq"}, nil
	}
	opts.QueuedCivmJobsFn = func(
		context.Context,
		[]string,
		time.Time,
		time.Duration,
	) (WatchdogQueueEvidence, error) {
		return WatchdogQueueEvidence{
			EligibleJobs: 16,
			Signature:    "oldest-advoq-job",
		}, nil
	}
	runnerBusy := false
	workerActive := false
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		if workerActive {
			return []idle.Activity{{PID: 42, Command: "Runner.Worker run"}}, nil
		}
		return nil, nil
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value":
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		case "gh api /orgs/advoq/actions/runners":
			return []byte(fmt.Sprintf(
				`{"runners":[{"id":1,"name":"civm-advoq-org","status":"online","busy":%t,"labels":[{"name":"self-hosted"},{"name":"civm"}]}]}`,
				runnerBusy,
			)), nil
		default:
			return []byte("active\n"), nil
		}
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		if strings.HasSuffix(path, "/.runner") {
			return []byte(`{"gitHubUrl":"https://github.com/advoq"}`), nil
		}
		return nil, os.ErrNotExist
	}
	restarts := 0
	opts.RestartFn = func(_ context.Context, unit string) error {
		if unit != "actions.runner.advoq.civm-advoq-org.service" {
			t.Fatalf("unexpected unit: %s", unit)
		}
		restarts++
		return nil
	}

	first := Watchdog(context.Background(), opts)
	if restarts != 0 || !hasWatchdogEvent(first, "queue-stall-armed") {
		t.Fatalf("first sample should only arm: restarts=%d events=%+v", restarts, first.Events)
	}
	if strings.Join(first.Repos, ",") != "advoq/advoq" {
		t.Fatalf("queue fleet missing from report: %+v", first.Repos)
	}
	if hasWatchdogEventWithReason(first, "rerun-skipped", "no-repos") {
		t.Fatalf("configured org fleet contradicted by no-repos: %+v", first.Events)
	}

	now = now.Add(6 * time.Minute)
	second := Watchdog(context.Background(), opts)
	if restarts != 1 || !hasWatchdogEventWithReason(second, "runner-restarted", "github-online-idle-with-civm-queue") {
		t.Fatalf("after dwell should restart once: restarts=%d events=%+v", restarts, second.Events)
	}

	now = now.Add(2 * time.Hour)
	third := Watchdog(context.Background(), opts)
	if restarts != 1 {
		t.Fatalf("same incident restarted %dx; want exactly once", restarts)
	}
	if !hasWatchdogEvent(third, "queue-stall-unresolved") || third.Exit != 2 {
		t.Fatalf("persistent incident should become critical and unresolved: %+v", third)
	}

	runnerBusy = true
	workerActive = true
	fourth := Watchdog(context.Background(), opts)
	if !hasWatchdogEventWithReason(fourth, "queue-stall-recovered", "runner-consuming-job") {
		t.Fatalf("a busy runner with a Worker should clear the incident: %+v", fourth.Events)
	}
	state, err := loadRerunState(opts)
	if err != nil {
		t.Fatalf("load state after recovery: %v", err)
	}
	if _, exists := state.QueueStalls["actions.runner.advoq.civm-advoq-org.service"]; exists {
		t.Fatalf("recovered queue stall remained persisted: %+v", state.QueueStalls)
	}
}

func TestWatchdogQueueStallFailsClosedWhenIdleUnknown(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.Repos = nil
	opts.InferRepos = true
	opts.IdleProbeDelay = 0
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.advoq.civm-advoq-org.service",
			Org:         "advoq",
			Name:        "civm-advoq-org",
			ActiveState: "active",
			SubState:    "running",
		}}, nil
	}
	opts.QueueReposFn = func() ([]string, error) { return []string{"advoq/advoq"}, nil }
	opts.QueuedCivmJobsFn = func(
		context.Context,
		[]string,
		time.Time,
		time.Duration,
	) (WatchdogQueueEvidence, error) {
		return WatchdogQueueEvidence{EligibleJobs: 1, Signature: "same-job"}, nil
	}
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return nil, errors.New("process probe failed")
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value":
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		case "gh api /orgs/advoq/actions/runners":
			return []byte(`{"runners":[{"name":"civm-advoq-org","status":"online","busy":false,"labels":[{"name":"self-hosted"},{"name":"civm"}]}]}`), nil
		default:
			return nil, nil
		}
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		return []byte(`{"gitHubUrl":"https://github.com/advoq"}`), nil
	}
	restarts := 0
	opts.RestartFn = func(context.Context, string) error { restarts++; return nil }

	report := Watchdog(context.Background(), opts)
	if restarts != 0 || !hasWatchdogEventWithReason(report, "runner-restart-skipped", "host-idle-unknown") {
		t.Fatalf("unknown idle state should fail closed: restarts=%d report=%+v", restarts, report)
	}
}

func TestWatchdogQueueStallRevalidatesIdleBeforeRestart(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	opts := baseWatchdogOptions(t)
	opts.Repos = nil
	opts.InferRepos = true
	opts.QueueStallDwell = time.Minute
	opts.IdleProbeDelay = 0
	opts.NowFn = func() time.Time { return now }
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.advoq.civm-advoq-org.service",
			Org:         "advoq",
			Name:        "civm-advoq-org",
			ActiveState: "active",
			SubState:    "running",
		}}, nil
	}
	opts.QueueReposFn = func() ([]string, error) { return []string{"advoq/advoq"}, nil }
	opts.QueuedCivmJobsFn = func(
		context.Context,
		[]string,
		time.Time,
		time.Duration,
	) (WatchdogQueueEvidence, error) {
		return WatchdogQueueEvidence{EligibleJobs: 1, Signature: "oldest"}, nil
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value":
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		case "gh api /orgs/advoq/actions/runners":
			return []byte(`{"runners":[{"name":"civm-advoq-org","status":"online","busy":false,"labels":[{"name":"self-hosted"},{"name":"civm"}]}]}`), nil
		default:
			return nil, nil
		}
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		return []byte(`{"gitHubUrl":"https://github.com/advoq"}`), nil
	}
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }
	Watchdog(context.Background(), opts)

	now = now.Add(2 * time.Minute)
	activityCalls := 0
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		activityCalls++
		if activityCalls >= 4 {
			return []idle.Activity{{PID: 99, Command: "Runner.Worker run"}}, nil
		}
		return nil, nil
	}
	restarts := 0
	opts.RestartFn = func(context.Context, string) error { restarts++; return nil }

	report := Watchdog(context.Background(), opts)
	if restarts != 0 || !hasWatchdogEventWithReason(report, "runner-restart-skipped", "host-busy") {
		t.Fatalf("Worker during revalidation should abort: restarts=%d events=%+v", restarts, report.Events)
	}
}

func TestListQueuedCivmJobsPaginatesFiltersAndSignsOldest(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	runFn := func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "gh api --paginate --slurp /repos/advoq/advoq/actions/runs?per_page=100&status=queued":
			return []byte(`[{"workflow_runs":[
			  {"id":10,"status":"queued","created_at":"2026-05-19T11:50:00Z"},
			  {"id":11,"status":"queued","created_at":"2026-05-19T11:49:00Z"},
			  {"id":12,"status":"queued","created_at":"2026-05-18T01:00:00Z"}
			]},{"workflow_runs":[
			  {"id":13,"status":"queued","created_at":"2026-05-19T11:55:00Z"}
			]}]`), nil
		case "gh api --paginate --slurp /repos/advoq/advoq/actions/runs/10/jobs?filter=latest&per_page=100":
			return []byte(`[{"jobs":[
			  {"id":100,"status":"queued","labels":["self-hosted","civm"]},
			  {"id":101,"status":"queued","labels":["ubuntu-latest"]}
			]},{"jobs":[{"id":103,"status":"in_progress","labels":["self-hosted","civm"]}]}]`), nil
		case "gh api --paginate --slurp /repos/advoq/advoq/actions/runs/11/jobs?filter=latest&per_page=100":
			return []byte(`[{"jobs":[{"id":102,"status":"in_progress","labels":["self-hosted","civm"]}]}]`), nil
		case "gh api --paginate --slurp /repos/advoq/docs/actions/runs?per_page=100&status=queued":
			return []byte(`[{"workflow_runs":[
			  {"id":20,"status":"queued","created_at":"2026-05-19T11:40:00Z"}
			]}]`), nil
		case "gh api --paginate --slurp /repos/advoq/docs/actions/runs/20/jobs?filter=latest&per_page=100":
			return []byte(`[{"jobs":[]},{"jobs":[
			  {"id":200,"status":"queued","labels":["civm","self-hosted"]}
			]}]`), nil
		default:
			t.Fatalf("unexpected endpoint: %s", call)
			return nil, nil
		}
	}

	got, err := listQueuedCivmJobs(
		context.Background(),
		[]string{"advoq/advoq", "advoq/docs"},
		now,
		6*time.Hour,
		runFn,
	)
	if err != nil {
		t.Fatalf("listQueuedCivmJobs: %v", err)
	}
	oldestIdentity := "advoq/docs/00000000000000000020/00000000000000000200"
	wantSignature := fmt.Sprintf("%x", sha256.Sum256([]byte(oldestIdentity)))
	if got.EligibleJobs != 2 || got.Signature != wantSignature {
		t.Fatalf("queue evidence = %+v, want jobs=2 signature=%s", got, wantSignature)
	}
}

func TestDefaultWatchdogQueueReposValidatesAndDeduplicates(t *testing.T) {
	t.Setenv("CIVM_REAPER_REPOS", "advoq/docs, advoq/advoq,advoq/docs")
	got, err := defaultWatchdogQueueRepos()
	if err != nil {
		t.Fatalf("defaultWatchdogQueueRepos: %v", err)
	}
	want := []string{"advoq/advoq", "advoq/docs"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("repos=%v, want=%v", got, want)
	}
}

func TestWatchdogOrgFleetResolutionFailsClosed(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.Repos = nil
	opts.InferRepos = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName: "actions.runner.advoq.civm-advoq-org.service",
			Org:      "advoq",
			Name:     "civm-advoq-org",
		}}, nil
	}
	opts.QueueReposFn = func() ([]string, error) {
		return nil, errors.New("fleet unreadable")
	}
	restarts := 0
	opts.RestartFn = func(context.Context, string) error {
		restarts++
		return nil
	}

	report := Watchdog(context.Background(), opts)
	if report.Exit != 2 || restarts != 0 {
		t.Fatalf("report=%+v restarts=%d, want exit 2 without mutation", report, restarts)
	}
	if !hasWatchdogEventWithReason(report, "runner-restart-skipped", "queue-repos-failed") {
		t.Fatalf("events=%+v, want queue-repos-failed", report.Events)
	}
}

func TestNormalizeWatchdogRepos(t *testing.T) {
	t.Parallel()
	got, err := normalizeWatchdogRepos([]string{" advoq/civm ", "advoq/advoq", "advoq/civm", ""})
	if err != nil {
		t.Fatalf("normalizeWatchdogRepos: %v", err)
	}
	if strings.Join(got, ",") != "advoq/advoq,advoq/civm" {
		t.Fatalf("repos=%v, want sorted unique fleet", got)
	}
	if _, err := normalizeWatchdogRepos([]string{"advoq"}); err == nil {
		t.Fatal("normalizeWatchdogRepos accepted repo without owner/name")
	}
}

func TestWatchdogRestartsOrgRunnerBusyWithoutLocalWorker(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	opts.IdleProbeDelay = 0
	opts.QueueStallDwell = 5 * time.Minute
	opts.NowFn = func() time.Time { return now }
	opts.Repos = nil
	opts.InferRepos = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.advoq.civm-advoq-org.service",
			Repo:        "advoq",
			Name:        "civm-advoq-org",
			ActiveState: "active",
			SubState:    "running",
		}}, nil
	}
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return nil, nil
	}
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value":
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		case "gh api /orgs/advoq/actions/runners":
			return []byte(`{"runners":[{"id":1,"name":"civm-advoq-org","status":"online","busy":true,"labels":[{"name":"self-hosted"},{"name":"civm"}]}]}`), nil
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			return []byte("active\n"), nil
		default:
			return nil, nil
		}
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		if !strings.HasSuffix(path, "/.runner") {
			return nil, errors.New("unexpected read: " + path)
		}
		return []byte(`{"gitHubUrl":"https://github.com/advoq"}`), nil
	}

	first := Watchdog(context.Background(), opts)
	if !hasWatchdogEventWithReason(first, "runner-restart-skipped", "org-busy-dwell") {
		t.Fatalf("first events = %+v, want dwell", first.Events)
	}
	now = now.Add(6 * time.Minute)
	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 events=%+v", report.Exit, report.Events)
	}
	assertWatchdogCall(t, calls, "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service")
	if !hasWatchdogEventWithReason(report, "runner-restarted", "github-busy-without-local-worker") {
		t.Fatalf("events = %+v, want stale busy restart", report.Events)
	}
}

func TestWatchdogSkipsOrgBusyRestartWhenWorkerAppears(t *testing.T) {
	t.Parallel()
	now := testWatchdogNow
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	opts.IdleProbeDelay = 0
	opts.QueueStallDwell = 5 * time.Minute
	opts.NowFn = func() time.Time { return now }
	opts.Repos = nil
	opts.InferRepos = true
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{{
			UnitName:    "actions.runner.advoq.civm-advoq-org.service",
			Repo:        "advoq",
			Name:        "civm-advoq-org",
			ActiveState: "active",
			SubState:    "running",
		}}, nil
	}
	activityCalls := 0
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		activityCalls++
		if activityCalls >= 3 {
			return []idle.Activity{{PID: 99, Command: "Runner.Worker run"}}, nil
		}
		return nil, nil
	}
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=WorkingDirectory --value":
			return []byte("/home/emedev/actions-runner-advoq-org\n"), nil
		case "gh api /orgs/advoq/actions/runners":
			return []byte(`{"runners":[{"id":1,"name":"civm-advoq-org","status":"online","busy":true,"labels":[{"name":"self-hosted"},{"name":"civm"}]}]}`), nil
		default:
			return nil, nil
		}
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if path == opts.MarkerPath {
			return os.ReadFile(path)
		}
		if !strings.HasSuffix(path, "/.runner") {
			return nil, errors.New("unexpected read: " + path)
		}
		return []byte(`{"gitHubUrl":"https://github.com/advoq"}`), nil
	}

	first := Watchdog(context.Background(), opts)
	if !hasWatchdogEventWithReason(first, "runner-restart-skipped", "org-busy-dwell") {
		t.Fatalf("first events = %+v, want dwell", first.Events)
	}
	activityCalls = 0
	now = now.Add(6 * time.Minute)
	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 events=%+v", report.Exit, report.Events)
	}
	if !hasWatchdogEventWithReason(report, "runner-restart-skipped", "host-busy") {
		t.Fatalf("events = %+v, want host-busy restart skip", report.Events)
	}
	for _, call := range calls {
		if strings.Contains(call, "systemctl restart") {
			t.Fatalf("watchdog restarted after worker appeared: %v", calls)
		}
	}
}

func TestDefaultWatchdogRestartFreezesBeforeFinalWorkerProbe(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	frozen := false
	restarts := 0
	thaws := 0
	activityCalls := 0
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "sudo systemctl freeze actions.runner.advoq.civm-advoq-org.service":
			frozen = true
			return nil, nil
		case "sudo systemctl thaw actions.runner.advoq.civm-advoq-org.service":
			frozen = false
			thaws++
			return nil, nil
		case "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service":
			restarts++
			return nil, nil
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			return []byte("active\n"), nil
		default:
			return nil, nil
		}
	}
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		activityCalls++
		if frozen {
			return []idle.Activity{{PID: 99, Command: "Runner.Worker spawnclient"}}, nil
		}
		return nil, nil
	}

	err := defaultWatchdogRestart(
		context.Background(),
		opts,
		"actions.runner.advoq.civm-advoq-org.service",
	)
	if err == nil {
		t.Fatal("defaultWatchdogRestart error = nil, want busy deferral")
	}
	if restarts != 0 {
		t.Fatalf("restarts = %d, want 0 after Worker appeared", restarts)
	}
	if activityCalls != 1 {
		t.Fatalf("activity calls = %d, want 1 under freeze", activityCalls)
	}
	if thaws != 1 || frozen {
		t.Fatalf("thaws = %d frozen = %t, want one thaw and unfrozen", thaws, frozen)
	}
}

func TestDefaultWatchdogRestartFreezesProbeThenStopsGracefully(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	frozen := false
	gracefullyStopped := false
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "sudo systemctl freeze actions.runner.advoq.civm-advoq-org.service":
			frozen = true
		case "sudo systemctl stop actions.runner.advoq.civm-advoq-org.service":
			if !frozen {
				t.Fatal("graceful stop ran without the listener fence")
			}
			frozen = false
			gracefullyStopped = true
		case "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service":
			if !gracefullyStopped {
				t.Fatal("restart ran before graceful stop")
			}
		case "sudo systemctl thaw actions.runner.advoq.civm-advoq-org.service":
			frozen = false
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			return []byte("active\n"), nil
		}
		return nil, nil
	}
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		if !frozen {
			t.Fatal("final activity probe ran without the listener fence")
		}
		return nil, nil
	}

	if err := defaultWatchdogRestart(
		context.Background(),
		opts,
		"actions.runner.advoq.civm-advoq-org.service",
	); err != nil {
		t.Fatalf("defaultWatchdogRestart: %v", err)
	}
	if frozen {
		t.Fatal("runner remained frozen after successful restart")
	}
	want := []string{
		"sudo systemctl freeze actions.runner.advoq.civm-advoq-org.service",
		"sudo systemctl stop actions.runner.advoq.civm-advoq-org.service",
		"sudo systemctl kill --kill-who=all --signal=SIGKILL actions.runner.advoq.civm-advoq-org.service",
		"sudo systemctl restart actions.runner.advoq.civm-advoq-org.service",
		"systemctl is-active actions.runner.advoq.civm-advoq-org.service",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestDefaultWatchdogRestartAllowsInactiveUnitWithoutFreeze(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	restarts := 0
	activityCalls := 0
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "sudo systemctl freeze actions.runner.advoq.civm-advoq-org.service":
			return nil, errors.New("unit is not running")
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			if restarts == 0 {
				return []byte("inactive\n"), errors.New("exit status 3")
			}
			return []byte("active\n"), nil
		case "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service":
			restarts++
		}
		return nil, nil
	}
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		activityCalls++
		return nil, nil
	}

	if err := defaultWatchdogRestart(
		context.Background(),
		opts,
		"actions.runner.advoq.civm-advoq-org.service",
	); err != nil {
		t.Fatalf("defaultWatchdogRestart: %v", err)
	}
	if restarts != 1 || activityCalls != 0 {
		t.Fatalf("restarts=%d activityCalls=%d, want 1/0", restarts, activityCalls)
	}
}

func TestDefaultWatchdogRestartPurgesFailedUnitCgroupBeforeRestart(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "sudo systemctl freeze actions.runner.advoq.civm-advoq-org.service":
			return nil, errors.New("unit is failed")
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			if slices.Contains(calls, "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service") {
				return []byte("active\n"), nil
			}
			return []byte("failed\n"), errors.New("exit status 3")
		}
		return nil, nil
	}

	if err := defaultWatchdogRestart(
		context.Background(),
		opts,
		"actions.runner.advoq.civm-advoq-org.service",
	); err != nil {
		t.Fatalf("defaultWatchdogRestart: %v", err)
	}
	kill := slices.Index(calls, "sudo systemctl kill --kill-who=all --signal=SIGKILL actions.runner.advoq.civm-advoq-org.service")
	restart := slices.Index(calls, "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service")
	if kill < 0 || restart < 0 || kill > restart {
		t.Fatalf("calls = %v, want cgroup kill before restart", calls)
	}
}

func TestDefaultWatchdogRestartFailsClosedWhenCgroupPurgeFails(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	restarts := 0
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "sudo systemctl freeze actions.runner.advoq.civm-advoq-org.service":
			return nil, errors.New("unit is failed")
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			return []byte("failed\n"), errors.New("exit status 3")
		case "sudo systemctl kill --kill-who=all --signal=SIGKILL actions.runner.advoq.civm-advoq-org.service":
			return nil, errors.New("access denied")
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=ControlGroup --value":
			return []byte("/system.slice/actions.runner.advoq.civm-advoq-org.service\n"), nil
		case "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service":
			restarts++
		}
		return nil, nil
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "/cgroup.procs") {
			return []byte("123\n"), nil
		}
		return nil, os.ErrNotExist
	}

	err := defaultWatchdogRestart(
		context.Background(),
		opts,
		"actions.runner.advoq.civm-advoq-org.service",
	)
	if err == nil || !strings.Contains(err.Error(), "systemctl kill") {
		t.Fatalf("error = %v, want cgroup purge failure", err)
	}
	if restarts != 0 {
		t.Fatalf("restarts = %d, want 0 after purge failure", restarts)
	}
}

func TestDefaultWatchdogRestartAcceptsKillRaceWhenCgroupIsEmpty(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	restarts := 0
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "sudo systemctl kill --kill-who=all --signal=SIGKILL actions.runner.advoq.civm-advoq-org.service":
			return nil, errors.New("exit status 1: Invalid argument")
		case "systemctl show actions.runner.advoq.civm-advoq-org.service --property=ControlGroup --value":
			return []byte("/system.slice/actions.runner.advoq.civm-advoq-org.service\n"), nil
		case "sudo systemctl restart actions.runner.advoq.civm-advoq-org.service":
			restarts++
		case "systemctl is-active actions.runner.advoq.civm-advoq-org.service":
			return []byte("active\n"), nil
		}
		return nil, nil
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "/cgroup.procs") {
			return nil, os.ErrNotExist
		}
		return nil, os.ErrNotExist
	}

	if err := defaultWatchdogRestart(
		context.Background(),
		opts,
		"actions.runner.advoq.civm-advoq-org.service",
	); err != nil {
		t.Fatalf("defaultWatchdogRestart: %v", err)
	}
	if restarts != 1 {
		t.Fatalf("restarts = %d, want 1 after empty cgroup proof", restarts)
	}
}

// TestWatchdogDoesNotResurrectRedundantRepoRunner prova a defesa de
// serialização: um runner por-repo (civm-app) já parado (inactive/dead, o
// estado pós-`systemctl disable`) NÃO pode ser reativado pelo watchdog enquanto
// o runner org (civm-acme-org) existe — senão a colisão do #1184 volta a cada
// tick de 2min. O watchdog declina o restart e emite runner-restart-skipped.
func TestWatchdogDoesNotResurrectRedundantRepoRunner(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RestartDelay = 0
	opts.Repos = []string{"acme/app"}
	const redundantUnit = "actions.runner.acme-app.civm-app.service"
	opts.SystemRunnersFn = func(context.Context) ([]Status, error) {
		return []Status{
			{UnitName: "actions.runner.acme.civm-acme-org.service", Repo: "acme", Name: "civm-acme-org", ActiveState: "active", SubState: "running"},
			{UnitName: redundantUnit, Repo: "acme/app", Name: "civm-app", ActiveState: "inactive", SubState: "dead"},
		}, nil
	}
	opts.GitHubRunnersFn = func(_ context.Context, repo string) ([]WatchdogGitHubRunner, error) {
		return []WatchdogGitHubRunner{{Repo: repo, Name: "civm-acme-org", Status: "online", Labels: []string{"self-hosted", "civm"}}}, nil
	}
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte("active\n"), nil
	}

	report := Watchdog(context.Background(), opts)

	if !hasWatchdogEventWithReason(report, "runner-restart-skipped", "redundant-repo-runner") {
		t.Fatalf("events = %+v, want runner-restart-skipped redundant-repo-runner", report.Events)
	}
	for _, call := range calls {
		if strings.Contains(call, "systemctl restart "+redundantUnit) {
			t.Fatalf("watchdog ressuscitou runner redundante: %q", call)
		}
	}
}

func TestClassifyFailureLogNetworkCheckout(t *testing.T) {
	t.Parallel()
	log := "Run actions/checkout@v5\nRPC failed; curl 56 GnuTLS recv error (-54)\nfatal: early EOF\ninvalid index-pack output"
	got := ClassifyFailureLog(log)
	if got.Kind != FailureNetworkCheckout {
		t.Fatalf("Kind = %s, want %s (%+v)", got.Kind, FailureNetworkCheckout, got)
	}
}

func TestClassifyFailureLogActionDownloadTransientServerFailure(t *testing.T) {
	t.Parallel()
	for _, serverFailure := range []string{"Internal Server Error", "Service Unavailable"} {
		serverFailure := serverFailure
		t.Run(serverFailure, func(t *testing.T) {
			t.Parallel()
			log := "Prepare all required actions\nGetting action download info\n" +
				"Failed to resolve action download info. Error: " + serverFailure
			got := ClassifyFailureLog(log)
			if got.Kind != FailureNetworkCheckout {
				t.Fatalf("Kind = %s, want %s (%+v)", got.Kind, FailureNetworkCheckout, got)
			}
		})
	}
}

func TestClassifyFailureLogActionDownloadWithoutTransientServerFailureIsUnknown(t *testing.T) {
	t.Parallel()
	log := "Prepare all required actions\n" +
		"Failed to resolve action download info. Error: action metadata is invalid\n" +
		"Run integration tests\nInternal Server Error"
	got := ClassifyFailureLog(log)
	if got.Kind != FailureUnknown {
		t.Fatalf("Kind = %s, want %s (%+v)", got.Kind, FailureUnknown, got)
	}
}

func TestClassifyFailureLogLintBeforeNetworkIsNotNetwork(t *testing.T) {
	t.Parallel()
	log := "Run golangci-lint run ./...\ninternal/foo.go:12:1: lint failed\nfatal: early EOF"
	got := ClassifyFailureLog(log)
	if got.Kind == FailureNetworkCheckout {
		t.Fatalf("Kind = %s, want non-network (%+v)", got.Kind, got)
	}
}

func TestWatchdogMarkerPreventsDuplicateRerun(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RerunNetworkFailures = true
	opts.ReadFileFn = func(string) ([]byte, error) {
		return []byte(`{"reruns":{"77/abc":{"repo":"acme/civm","run_id":77,"head_sha":"abc","rerun_at":"2026-05-19T12:00:00Z"}}}`), nil
	}
	rerunCalls := 0
	opts.ListRunsFn = func(context.Context, string, int) ([]WatchdogRun, error) {
		return []WatchdogRun{{
			ID:           77,
			HeadSHA:      "abc",
			Conclusion:   "failure",
			CreatedAt:    testWatchdogNow.Add(-time.Hour),
			PullRequests: []WatchdogPullRequestRef{{Number: 7}},
		}}, nil
	}
	opts.PullRequestFn = func(context.Context, string, int) (WatchdogPullRequest, error) {
		return WatchdogPullRequest{Number: 7, State: "open", MergeableState: "clean"}, nil
	}
	logCalls := 0
	opts.RunLogFn = func(context.Context, string, int64) (string, error) {
		logCalls++
		return "Run actions/checkout@v5\nfatal: early EOF\ninvalid index-pack output", nil
	}
	opts.RerunFn = func(context.Context, string, int64) error {
		rerunCalls++
		return nil
	}

	report := Watchdog(context.Background(), opts)
	if rerunCalls != 0 {
		t.Fatalf("rerunCalls = %d, want 0", rerunCalls)
	}
	if logCalls != 0 {
		t.Fatalf("logCalls = %d, want 0", logCalls)
	}
	if !hasWatchdogEventWithReason(report, "rerun-skipped", "already-rerun") {
		t.Fatalf("events = %+v, want rerun-skipped already-rerun", report.Events)
	}
}

func TestWatchdogTriggersNetworkRerunAndWritesMarker(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RerunNetworkFailures = true
	written := ""
	reruns := []int64{}
	opts.ListRunsFn = func(context.Context, string, int) ([]WatchdogRun, error) {
		return []WatchdogRun{{
			ID:           99,
			HeadSHA:      "abc123",
			Conclusion:   "timed_out",
			CreatedAt:    testWatchdogNow.Add(-time.Hour),
			PullRequests: []WatchdogPullRequestRef{{Number: 7}},
		}}, nil
	}
	opts.PullRequestFn = func(context.Context, string, int) (WatchdogPullRequest, error) {
		return WatchdogPullRequest{Number: 7, State: "open", MergeableState: "clean"}, nil
	}
	opts.RunLogFn = func(context.Context, string, int64) (string, error) {
		return "Prepare all required actions\nGetting action download info\n" +
			"Failed to resolve action download info. Error: Service Unavailable\n", nil
	}
	opts.RerunFn = func(_ context.Context, repo string, runID int64) error {
		if repo != "acme/civm" {
			t.Fatalf("repo = %q", repo)
		}
		reruns = append(reruns, runID)
		return nil
	}
	opts.WriteFileFn = func(_ string, data []byte, _ os.FileMode) error {
		written = string(data)
		return nil
	}
	opts.RenameFn = func(string, string) error { return nil }

	report := Watchdog(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 events=%+v", report.Exit, report.Events)
	}
	if len(reruns) != 1 || reruns[0] != 99 {
		t.Fatalf("reruns = %v, want [99]", reruns)
	}
	if !hasWatchdogEvent(report, "rerun-triggered") {
		t.Fatalf("events = %+v, want rerun-triggered", report.Events)
	}
	if report.Metrics.RunsConsidered != 1 || report.Metrics.RerunsTriggered != 1 || report.Metrics.RerunsSkipped != 0 {
		t.Fatalf("metrics = %+v, want considered=1 triggered=1 skipped=0", report.Metrics)
	}
	if !strings.Contains(written, `"99/abc123"`) {
		t.Fatalf("marker not written for run/head: %s", written)
	}
}

func TestWatchdogSkipsOldRunBeforePRAndLog(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RerunNetworkFailures = true
	prCalls := 0
	logCalls := 0
	opts.ListRunsFn = func(context.Context, string, int) ([]WatchdogRun, error) {
		return []WatchdogRun{{
			ID:           101,
			HeadSHA:      "old",
			Conclusion:   "failure",
			CreatedAt:    testWatchdogNow.Add(-7 * time.Hour),
			PullRequests: []WatchdogPullRequestRef{{Number: 7}},
		}}, nil
	}
	opts.PullRequestFn = func(context.Context, string, int) (WatchdogPullRequest, error) {
		prCalls++
		return WatchdogPullRequest{Number: 7, State: "open", MergeableState: "clean"}, nil
	}
	opts.RunLogFn = func(context.Context, string, int64) (string, error) {
		logCalls++
		return "fatal: early EOF", nil
	}

	report := Watchdog(context.Background(), opts)
	if prCalls != 0 || logCalls != 0 {
		t.Fatalf("old run reached PR/log: pr=%d log=%d", prCalls, logCalls)
	}
	if !hasWatchdogEventWithReason(report, "rerun-skipped", "run-too-old") {
		t.Fatalf("events = %+v, want rerun-skipped run-too-old", report.Events)
	}
	if report.Metrics.RunsConsidered != 1 || report.Metrics.RerunsSkipped != 1 {
		t.Fatalf("metrics = %+v, want considered=1 skipped=1", report.Metrics)
	}
}

func TestWatchdogSkipsMissingCreatedAtBeforePRAndLog(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.RerunNetworkFailures = true
	prCalls := 0
	logCalls := 0
	opts.ListRunsFn = func(context.Context, string, int) ([]WatchdogRun, error) {
		return []WatchdogRun{{
			ID:           102,
			HeadSHA:      "missing",
			Conclusion:   "failure",
			PullRequests: []WatchdogPullRequestRef{{Number: 7}},
		}}, nil
	}
	opts.PullRequestFn = func(context.Context, string, int) (WatchdogPullRequest, error) {
		prCalls++
		return WatchdogPullRequest{Number: 7, State: "open", MergeableState: "clean"}, nil
	}
	opts.RunLogFn = func(context.Context, string, int64) (string, error) {
		logCalls++
		return "fatal: early EOF", nil
	}

	report := Watchdog(context.Background(), opts)
	if prCalls != 0 || logCalls != 0 {
		t.Fatalf("missing created_at reached PR/log: pr=%d log=%d", prCalls, logCalls)
	}
	if !hasWatchdogEventWithReason(report, "rerun-skipped", "run-created-at-missing") {
		t.Fatalf("events = %+v, want rerun-skipped run-created-at-missing", report.Events)
	}
	if report.Metrics.RunsConsidered != 1 || report.Metrics.RerunsSkipped != 1 {
		t.Fatalf("metrics = %+v, want considered=1 skipped=1", report.Metrics)
	}
}

func TestWatchdogRerunMetricsCountDryRunTriggerDecision(t *testing.T) {
	t.Parallel()
	opts := baseWatchdogOptions(t)
	opts.Execute = false
	opts.RerunNetworkFailures = true
	opts.ListRunsFn = func(context.Context, string, int) ([]WatchdogRun, error) {
		return []WatchdogRun{
			{ID: 1, HeadSHA: "missing", Conclusion: "failure", PullRequests: []WatchdogPullRequestRef{{Number: 7}}},
			{ID: 2, HeadSHA: "code", Conclusion: "failure", CreatedAt: testWatchdogNow.Add(-time.Hour), PullRequests: []WatchdogPullRequestRef{{Number: 7}}},
			{ID: 3, HeadSHA: "network", Conclusion: "failure", CreatedAt: testWatchdogNow.Add(-time.Hour), PullRequests: []WatchdogPullRequestRef{{Number: 7}}},
		}, nil
	}
	opts.PullRequestFn = func(context.Context, string, int) (WatchdogPullRequest, error) {
		return WatchdogPullRequest{Number: 7, State: "open", MergeableState: "clean"}, nil
	}
	opts.RunLogFn = func(_ context.Context, _ string, runID int64) (string, error) {
		if runID == 2 {
			return "Run go test ./...\ntests failed\nfatal: early EOF", nil
		}
		return "Run actions/checkout@v5\ncurl 56\nfatal: early EOF", nil
	}
	rerunCalls := 0
	opts.RerunFn = func(context.Context, string, int64) error {
		rerunCalls++
		return nil
	}

	report := Watchdog(context.Background(), opts)
	if rerunCalls != 0 {
		t.Fatalf("dry-run rerunCalls = %d, want 0", rerunCalls)
	}
	if report.Metrics.RunsConsidered != 3 || report.Metrics.RerunsTriggered != 1 || report.Metrics.RerunsSkipped != 2 {
		t.Fatalf("metrics = %+v, want considered=3 triggered=1 skipped=2", report.Metrics)
	}
	for _, event := range report.Events {
		if event.Event == "rerun-triggered" && event.Executed {
			t.Fatalf("dry-run rerun-triggered event executed=true: %+v", event)
		}
	}
}

func TestWatchdogSkipsClosedOrConflictingPR(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pr   WatchdogPullRequest
		want string
	}{
		{name: "closed", pr: WatchdogPullRequest{Number: 7, State: "closed", MergeableState: "clean"}, want: "pr-not-open"},
		{name: "conflicting", pr: WatchdogPullRequest{Number: 7, State: "open", MergeableState: "dirty"}, want: "pr-conflicting"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := baseWatchdogOptions(t)
			opts.RerunNetworkFailures = true
			rerunCalls := 0
			opts.ListRunsFn = func(context.Context, string, int) ([]WatchdogRun, error) {
				return []WatchdogRun{{
					ID:           88,
					HeadSHA:      "abc",
					Conclusion:   "failure",
					CreatedAt:    testWatchdogNow.Add(-time.Hour),
					PullRequests: []WatchdogPullRequestRef{{Number: 7}},
				}}, nil
			}
			opts.PullRequestFn = func(context.Context, string, int) (WatchdogPullRequest, error) {
				return tt.pr, nil
			}
			opts.RunLogFn = func(context.Context, string, int64) (string, error) {
				return "Run actions/checkout@v5\nConnection timed out\n", nil
			}
			opts.RerunFn = func(context.Context, string, int64) error {
				rerunCalls++
				return nil
			}

			report := Watchdog(context.Background(), opts)
			if rerunCalls != 0 {
				t.Fatalf("rerunCalls = %d, want 0", rerunCalls)
			}
			if !hasWatchdogEventWithReason(report, "rerun-skipped", tt.want) {
				t.Fatalf("events = %+v, want rerun-skipped %s", report.Events, tt.want)
			}
		})
	}
}

func TestWatchdogInfersHyphenatedRepoFromRunnerConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := DefaultWatchdogOptions()
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "systemctl show actions.runner.acme-org-deep-repo-name.civm-acme.service --property=WorkingDirectory --value" {
			return []byte(dir + "\n"), nil
		}
		return nil, errors.New("unexpected call: " + call)
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		if !strings.HasSuffix(path, "/.runner") {
			return nil, errors.New("unexpected read: " + path)
		}
		return []byte(`{"gitHubUrl":"https://github.com/acme-org/deep-repo-name"}`), nil
	}
	systemd := []Status{{
		UnitName: "actions.runner.acme-org-deep-repo-name.civm-acme.service",
		Repo:     "acme/org-deep-repo-name",
		Name:     "civm-acme",
	}}

	repos := inferWatchdogRepos(enrichWatchdogSystemdRepos(context.Background(), opts, systemd))
	if strings.Join(repos, ",") != "acme-org/deep-repo-name" {
		t.Fatalf("repos = %v, want acme-org/deep-repo-name", repos)
	}
}

func TestWatchdogInferReposFallsBackToUnitParser(t *testing.T) {
	t.Parallel()
	opts := DefaultWatchdogOptions()
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("systemctl show unavailable")
	}
	systemd := []Status{{
		UnitName: "actions.runner.owner-repo-with-hyphen.civm.service",
		Repo:     "owner/repo-with-hyphen",
		Name:     "civm",
	}}

	repos := inferWatchdogRepos(enrichWatchdogSystemdRepos(context.Background(), opts, systemd))
	if strings.Join(repos, ",") != "owner/repo-with-hyphen" {
		t.Fatalf("repos = %v, want fallback owner/repo-with-hyphen", repos)
	}
}

func TestApplyWatchdogDefaultsInstallsCommandBackedFunctions(t *testing.T) {
	t.Parallel()
	opts := WatchdogOptions{
		RunFn: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "git" && strings.Join(args, " ") == "ls-remote https://github.com/actions/checkout.git HEAD" {
				return []byte("abc\tHEAD\n"), nil
			}
			if name == "systemctl" && strings.Join(args, " ") == "list-units --type=service --no-pager --no-legend --all actions.runner.*" {
				return []byte(fakeSystemctlOutput), nil
			}
			if name == "gh" && strings.Join(args, " ") == "api /repos/acme/civm/actions/runners" {
				return []byte(`{"runners":[{"id":1,"name":"civm-self","status":"online","busy":false,"labels":[{"name":"self-hosted"},{"name":"civm"}]}]}`), nil
			}
			if name == "gh" && strings.Join(args, " ") == "api /repos/acme/civm/actions/runs?per_page=20&status=completed" {
				return []byte(`{"workflow_runs":[{"id":8,"head_sha":"abc","status":"completed","conclusion":"failure","created_at":"2026-05-19T12:00:00Z","html_url":"https://github.com/advoq/civm/actions/runs/8","pull_requests":[{"number":7}]}]}`), nil
			}
			if name == "gh" && strings.Join(args, " ") == "api /repos/acme/civm/pulls/7" {
				return []byte(`{"number":7,"state":"open","mergeable_state":"clean"}`), nil
			}
			if name == "gh" && strings.Join(args, " ") == "run view 8 --repo acme/civm --log-failed" {
				return []byte("Run actions/checkout@v5\nfatal: early EOF\n"), nil
			}
			if name == "gh" && strings.Join(args, " ") == "run rerun 8 --repo acme/civm --failed" {
				return []byte(""), nil
			}
			return nil, errors.New("unexpected call: " + name + " " + strings.Join(args, " "))
		},
	}
	applyWatchdogDefaults(&opts)

	if err := opts.NetworkFn(context.Background(), time.Second); err != nil {
		t.Fatalf("NetworkFn error: %v", err)
	}
	statuses, err := opts.SystemRunnersFn(context.Background())
	if err != nil {
		t.Fatalf("SystemRunnersFn error: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("statuses=%d, want 3", len(statuses))
	}
	runners, err := opts.GitHubRunnersFn(context.Background(), "acme/civm")
	if err != nil {
		t.Fatalf("GitHubRunnersFn error: %v", err)
	}
	if len(runners) != 1 || runners[0].Name != "civm-self" || !hasWatchdogLabel(runners[0].Labels, "civm") {
		t.Fatalf("runners=%+v, want civm runner with label", runners)
	}
	runs, err := opts.ListRunsFn(context.Background(), "acme/civm", opts.RunLimit)
	if err != nil {
		t.Fatalf("ListRunsFn error: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != 8 || len(runs[0].PullRequests) != 1 {
		t.Fatalf("runs=%+v, want parsed run with PR", runs)
	}
	pr, err := opts.PullRequestFn(context.Background(), "acme/civm", 7)
	if err != nil {
		t.Fatalf("PullRequestFn error: %v", err)
	}
	if pr.State != "open" || pr.MergeableState != "clean" {
		t.Fatalf("pr=%+v, want open clean", pr)
	}
	log, err := opts.RunLogFn(context.Background(), "acme/civm", 8)
	if err != nil {
		t.Fatalf("RunLogFn error: %v", err)
	}
	if !strings.Contains(log, "early EOF") {
		t.Fatalf("log=%q, want checkout failure", log)
	}
	if err := opts.RerunFn(context.Background(), "acme/civm", 8); err != nil {
		t.Fatalf("RerunFn error: %v", err)
	}
}

func TestWatchdogCLIParsersRejectInvalidJSON(t *testing.T) {
	t.Parallel()
	runFn := func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{`), nil
	}

	if _, err := listWatchdogGitHubRunners(context.Background(), "acme/civm", runFn); err == nil {
		t.Fatal("listWatchdogGitHubRunners error=nil, want parse error")
	}
	if _, err := listWatchdogRuns(context.Background(), "acme/civm", 5, runFn); err == nil {
		t.Fatal("listWatchdogRuns error=nil, want parse error")
	}
	if _, err := getWatchdogPullRequest(context.Background(), "acme/civm", 7, runFn); err == nil {
		t.Fatal("getWatchdogPullRequest error=nil, want parse error")
	}
}

func TestValidateWatchdogOptionsRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*WatchdogOptions)
	}{
		{name: "repo", edit: func(o *WatchdogOptions) { o.Repos = []string{"bad"} }},
		{name: "network-timeout", edit: func(o *WatchdogOptions) { o.NetworkTimeout = 0 }},
		{name: "restart-delay", edit: func(o *WatchdogOptions) { o.RestartDelay = -time.Second }},
		{name: "max-run-age", edit: func(o *WatchdogOptions) { o.MaxRunAge = 0 }},
		{name: "run-limit-low", edit: func(o *WatchdogOptions) { o.RunLimit = 0 }},
		{name: "run-limit-high", edit: func(o *WatchdogOptions) { o.RunLimit = 101 }},
		{name: "marker-path", edit: func(o *WatchdogOptions) { o.MarkerPath = "relative.json" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := WatchdogOptions{
				Repos:          []string{"acme/civm"},
				NetworkTimeout: time.Second,
				RestartDelay:   0,
				MaxRunAge:      time.Hour,
				RunLimit:       10,
				MarkerPath:     "/tmp/runner-watchdog.json",
			}
			tt.edit(&opts)
			if err := validateWatchdogOptions(opts); err == nil {
				t.Fatal("validateWatchdogOptions error=nil, want error")
			}
		})
	}
}

func TestWatchdogReportRenderers(t *testing.T) {
	t.Parallel()
	report := WatchdogReport{
		Executed:     true,
		Repos:        []string{"acme/civm"},
		RunnerOnline: true,
		Metrics:      WatchdogMetrics{RunsConsidered: 2, RerunsTriggered: 1, RerunsSkipped: 1},
		Events: []WatchdogEvent{
			{Event: "runner-online", Severity: "info", Repo: "acme/civm", Runner: "civm-self", Online: true},
			{Event: "rerun-triggered", Severity: "info", Repo: "acme/civm", RunID: 8, Reason: "network-checkout", Detail: "signature=early eof"},
		},
		Exit: 0,
	}

	var jsonBuf bytes.Buffer
	if err := report.RenderJSON(&jsonBuf); err != nil {
		t.Fatalf("RenderJSON error: %v", err)
	}
	var parsed WatchdogReport
	if err := json.Unmarshal(jsonBuf.Bytes(), &parsed); err != nil {
		t.Fatalf("RenderJSON produced invalid JSON: %v", err)
	}
	if parsed.Metrics.RerunsTriggered != 1 {
		t.Fatalf("parsed metrics=%+v, want trigger count", parsed.Metrics)
	}

	var textBuf bytes.Buffer
	report.Render(&textBuf)
	out := textBuf.String()
	for _, want := range []string{"EXECUTE", "runner_online=true", "Repos: acme/civm", "rerun-triggered", "network-checkout: signature=early eof"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Render output missing %q:\n%s", want, out)
		}
	}
}

func hasWatchdogEvent(report WatchdogReport, event string) bool {
	for _, item := range report.Events {
		if item.Event == event {
			return true
		}
	}
	return false
}

func hasWatchdogEventWithReason(report WatchdogReport, event, reason string) bool {
	for _, item := range report.Events {
		if item.Event == event && item.Reason == reason {
			return true
		}
	}
	return false
}

func assertWatchdogCall(t *testing.T, calls []string, want string) {
	t.Helper()
	for _, call := range calls {
		if call == want {
			return
		}
	}
	t.Fatalf("missing call %q in %v", want, calls)
}
