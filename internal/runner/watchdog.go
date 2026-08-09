package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/advoq/civm/internal/civm"
	"github.com/advoq/civm/internal/hook"
	"github.com/advoq/civm/internal/idle"
)

const (
	DefaultWatchdogMarkerPath = civm.DefaultRunnerWatchdogMarkerPath
	DefaultWatchdogMaxRunAge  = 6 * time.Hour
	DefaultQueueStallDwell    = 5 * time.Minute
	defaultWatchdogRunLimit   = 20
)

var errWatchdogRunnerBecameBusy = errors.New("runner became busy while restart was fenced")

type WatchdogOptions struct {
	Execute              bool
	Repos                []string
	InferRepos           bool
	RerunNetworkFailures bool
	NetworkTimeout       time.Duration
	RestartDelay         time.Duration
	IdleProbeDelay       time.Duration
	MaxRunAge            time.Duration
	RunLimit             int
	MarkerPath           string
	MaintenanceStatePath string
	HooksDir             string
	RunnerGlob           string
	CivmctlPath          string
	HooksLogPath         string // shared hooks.jsonl tail read for the broken-runner sentinel (ITEM-10)
	AutoRestartPerHour   int    // cap of sentinel-driven auto-restarts per unit per rolling hour
	QueueStallDwell      time.Duration
	RunFn                func(ctx context.Context, name string, args ...string) ([]byte, error)
	NetworkFn            func(ctx context.Context, timeout time.Duration) error
	ActivityFn           func(ctx context.Context) ([]idle.Activity, error)
	SystemRunnersFn      func(ctx context.Context) ([]Status, error)
	GitHubRunnersFn      func(ctx context.Context, repo string) ([]WatchdogGitHubRunner, error)
	HookInstallFn        func(ctx context.Context, opts hook.InstallOptions) hook.InstallResult
	ListRunsFn           func(ctx context.Context, repo string, limit int) ([]WatchdogRun, error)
	PullRequestFn        func(ctx context.Context, repo string, number int) (WatchdogPullRequest, error)
	RunLogFn             func(ctx context.Context, repo string, runID int64) (string, error)
	RerunFn              func(ctx context.Context, repo string, runID int64) error
	QueueReposFn         func() ([]string, error)
	QueuedCivmJobsFn     func(ctx context.Context, repos []string, now time.Time, maxAge time.Duration) (WatchdogQueueEvidence, error)
	ReadFileFn           func(path string) ([]byte, error)
	WriteFileFn          func(path string, data []byte, perm os.FileMode) error
	MkdirAllFn           func(path string, perm os.FileMode) error
	RenameFn             func(oldPath, newPath string) error
	RemoveFn             func(path string) error
	MaintenanceActiveFn  func(path string) (bool, error)
	NowFn                func() time.Time
	SleepFn              func(d time.Duration)
	// RestartFn restarts a single broken runner unit. The production default
	// fences the target unit and rechecks Runner.Worker before mutating, closing
	// the listener-admission TOCTOU window. Injected for hermetic tests.
	RestartFn func(ctx context.Context, unit string) error
}

type WatchdogReport struct {
	Executed     bool            `json:"executed"`
	Repos        []string        `json:"repos"`
	RunnerOnline bool            `json:"runner_online"`
	Metrics      WatchdogMetrics `json:"metrics"`
	Events       []WatchdogEvent `json:"events"`
	Exit         int             `json:"exit"`
}

type WatchdogMetrics struct {
	RunsConsidered   int `json:"runs_considered"`
	RerunsTriggered  int `json:"reruns_triggered"`
	RerunsSkipped    int `json:"reruns_skipped"`
	QueueStalls      int `json:"queue_stalls"`
	RunnerRestarts   int `json:"runner_restarts"`
	StallsUnresolved int `json:"stalls_unresolved"`
}

type WatchdogEvent struct {
	Event      string `json:"event"`
	Severity   string `json:"severity"`
	Repo       string `json:"repo,omitempty"`
	Unit       string `json:"unit,omitempty"`
	Runner     string `json:"runner,omitempty"`
	RunID      int64  `json:"run_id,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Executed   bool   `json:"executed"`
	Online     bool   `json:"online,omitempty"`
	QueuedJobs int    `json:"queued_jobs,omitempty"`
	AgeSeconds int64  `json:"age_seconds,omitempty"`
}

type WatchdogQueueEvidence struct {
	EligibleJobs int
	Signature    string
}

type watchdogQueuedRun struct {
	ID        int64
	CreatedAt time.Time
}

type watchdogQueuedCandidate struct {
	CreatedAt time.Time
	Identity  string
}

type WatchdogGitHubRunner struct {
	ID     int64    `json:"id,omitempty"`
	Repo   string   `json:"repo"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Busy   bool     `json:"busy"`
	Labels []string `json:"labels,omitempty"`
}

type WatchdogRun struct {
	ID           int64                    `json:"id"`
	HeadSHA      string                   `json:"head_sha"`
	Status       string                   `json:"status"`
	Conclusion   string                   `json:"conclusion"`
	CreatedAt    time.Time                `json:"created_at,omitempty"`
	URL          string                   `json:"url,omitempty"`
	PullRequests []WatchdogPullRequestRef `json:"pull_requests,omitempty"`
}

type WatchdogPullRequestRef struct {
	Number int `json:"number"`
}

type WatchdogPullRequest struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	MergeableState string `json:"mergeable_state"`
}

type FailureKind string

const (
	FailureUnknown         FailureKind = "unknown"
	FailureNetworkCheckout FailureKind = "network_checkout"
	FailureCode            FailureKind = "code"
	FailureSecret          FailureKind = "secret"
)

type FailureClassification struct {
	Kind      FailureKind `json:"kind"`
	Signature string      `json:"signature,omitempty"`
	Detail    string      `json:"detail,omitempty"`
}

func DefaultWatchdogOptions() WatchdogOptions {
	return WatchdogOptions{
		Execute:              false,
		InferRepos:           true,
		NetworkTimeout:       10 * time.Second,
		RestartDelay:         10 * time.Second,
		IdleProbeDelay:       2 * time.Second,
		MaxRunAge:            DefaultWatchdogMaxRunAge,
		RunLimit:             defaultWatchdogRunLimit,
		MarkerPath:           DefaultWatchdogMarkerPath,
		MaintenanceStatePath: civm.DefaultMaintenanceStatePath,
		HooksLogPath:         civm.DefaultHooksLogPath,
		AutoRestartPerHour:   civm.DefaultRunnerAutoRestartPerHour,
		QueueStallDwell:      DefaultQueueStallDwell,
		RunFn:                defaultRun,
		ActivityFn:           idle.DefaultActivities,
		ReadFileFn:           os.ReadFile,
		WriteFileFn:          os.WriteFile,
		MkdirAllFn:           os.MkdirAll,
		RenameFn:             os.Rename,
		RemoveFn:             os.Remove,
		MaintenanceActiveFn:  defaultMaintenanceActive,
		NowFn:                time.Now,
		SleepFn:              time.Sleep,
		HookInstallFn:        hook.Install,
	}
}

func Watchdog(ctx context.Context, opts WatchdogOptions) WatchdogReport {
	applyWatchdogDefaults(&opts)
	report := WatchdogReport{Executed: opts.Execute, Exit: 0}
	if err := validateWatchdogOptions(opts); err != nil {
		report.add(WatchdogEvent{Event: "watchdog-invalid", Severity: "critical", Reason: "invalid-options", Detail: err.Error()})
		report.Exit = 2
		return report
	}
	maintenanceActive, err := opts.MaintenanceActiveFn(opts.MaintenanceStatePath)
	if err != nil {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "critical",
			Reason: "maintenance-state-unknown", Detail: err.Error(),
		})
		report.Exit = 1
		return report
	}
	if maintenanceActive {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "info",
			Reason: "maintenance-active",
		})
		return report
	}
	if err := opts.NetworkFn(ctx, opts.NetworkTimeout); err != nil {
		report.add(WatchdogEvent{Event: "network-down", Severity: "warning", Reason: "github-unreachable", Detail: err.Error()})
		report.Exit = 1
		return report
	}

	systemd, err := opts.SystemRunnersFn(ctx)
	if err != nil {
		report.add(WatchdogEvent{Event: "runner-status-unknown", Severity: "warning", Reason: "systemd-list-failed", Detail: err.Error()})
		report.Exit = maxExit(report.Exit, 1)
	}
	systemd = enrichWatchdogSystemdRepos(ctx, opts, systemd)
	repos := append([]string(nil), opts.Repos...)
	if opts.InferRepos && len(repos) == 0 {
		repos = inferWatchdogRepos(systemd)
	}
	usedOrgFleetFallback := false
	if opts.InferRepos && len(repos) == 0 && hasWatchdogOrgRunner(systemd) {
		configuredRepos, configErr := opts.QueueReposFn()
		if configErr != nil {
			report.add(WatchdogEvent{
				Event: "runner-restart-skipped", Severity: "critical",
				Reason: "queue-repos-failed", Detail: configErr.Error(),
			})
			report.Exit = 2
			return report
		}
		repos, err = normalizeWatchdogRepos(configuredRepos)
		if err != nil {
			report.add(WatchdogEvent{
				Event: "runner-restart-skipped", Severity: "critical",
				Reason: "queue-repos-failed", Detail: err.Error(),
			})
			report.Exit = 2
			return report
		}
		usedOrgFleetFallback = len(repos) > 0
	}
	report.Repos = repos
	if len(repos) == 0 || usedOrgFleetFallback {
		localOnlineBeforeRepair := anyLocalRunnerOnline(systemd)
		recoverWatchdogOrgBusyWithoutWorker(ctx, opts, systemd, &report)
		recoverWatchdogOrgIdleWithQueue(ctx, opts, systemd, repos, &report)
		if err := restartWatchdogRunners(ctx, opts, systemd, nil, &report); err != nil {
			report.Exit = 2
			return report
		}
		report.RunnerOnline = localOnlineBeforeRepair || watchdogReportHasEvent(report, "runner-restarted")
		if len(repos) == 0 {
			report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "warning", Reason: "no-repos"})
		}
		if len(systemd) == 0 {
			report.Exit = maxExit(report.Exit, 1)
		}
		return report
	}

	ghBefore, repoOnline := collectWatchdogGitHubRunners(ctx, opts, repos, &report)
	report.RunnerOnline = anyRepoOnline(repoOnline)

	// Auto-recover a wedged runner BEFORE the idle skip: a broken-runner sentinel
	// in hooks.jsonl means that unit is stuck and must restart even while other
	// units are busy. Sentinel-gated + per-unit hourly cap (RF-6 / ITEM-10).
	// In dry-run it only surfaces candidates; it restarts only with Execute.
	detectBrokenRunner(ctx, opts, systemd, &report)

	idleResult := idle.Result{Status: idle.StatusIdle, ExitCode: idle.StatusIdle.ExitCode()}
	if opts.Execute {
		idleResult = idle.Check(ctx, idle.Options{ActivityFn: opts.ActivityFn, ProbeDelay: opts.IdleProbeDelay})
		if idleResult.Status != idle.StatusIdle {
			reason := "host-busy"
			detail := idle.FormatActivities(idleResult.Activities)
			if idleResult.Status == idle.StatusUnknown {
				reason = "host-idle-unknown"
				detail = idleResult.Error
			}
			report.add(WatchdogEvent{Event: "runner-restart-skipped", Severity: "warning", Reason: reason, Detail: detail})
			if opts.RerunNetworkFailures {
				report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "warning", Reason: reason, Detail: detail})
			}
			// A busy host (and a probe that cannot prove idle) is the expected
			// steady state on a shared runner box: deferring maintenance to the
			// next idle tick is correct behavior, not a watchdog failure. The
			// warning event above preserves observability. Marking the systemd
			// unit failed on every tick (the box is busy most of the time) would
			// only keep it perpetually red and mask genuine faults. Non-zero is
			// reserved for real failures: hook-install / runner-restart errors
			// (exit 2) and a runner that stays offline after repair (exit 1).
			// Ref: docs/specs/civm-runner-reliability/SPEC.md RF-6 / ITEM-10.
			return report
		}
	}

	if err := repairWatchdogHooks(ctx, opts, &report); err != nil {
		report.Exit = 2
		return report
	}
	if err := restartWatchdogRunners(ctx, opts, systemd, ghBefore, &report); err != nil {
		report.Exit = 2
		return report
	}

	_, repoOnline = collectWatchdogGitHubRunners(ctx, opts, repos, &report)
	report.RunnerOnline = anyRepoOnline(repoOnline)
	for _, repo := range repos {
		if !repoOnline[repo] {
			report.add(WatchdogEvent{Event: "runner-offline", Severity: "warning", Repo: repo, Reason: "github-not-online"})
			report.Exit = maxExit(report.Exit, 1)
		}
	}

	if !opts.RerunNetworkFailures {
		report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "info", Reason: "disabled"})
		return report
	}
	if err := rerunWatchdogNetworkFailures(ctx, opts, repos, repoOnline, &report); err != nil {
		report.Exit = 2
	}
	return report
}

func defaultMaintenanceActive(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func repairWatchdogHooks(ctx context.Context, opts WatchdogOptions, report *WatchdogReport) error {
	hookOpts := hook.DefaultInstallOptions()
	hookOpts.Execute = opts.Execute
	hookOpts.RestartRunners = false
	// The watchdog is a light-touch periodic .env repair. The privileged
	// safedelete wrapper + scoped sudoers are a one-time provisioning concern
	// (hook install --execute / bootstrap-everything), not something to re-run
	// (visudo + /etc/sudoers.d rewrite) on every timer tick.
	hookOpts.SkipScopedSudoers = true
	if opts.HooksDir != "" {
		hookOpts.HooksDir = opts.HooksDir
	}
	if opts.RunnerGlob != "" {
		hookOpts.RunnerGlob = opts.RunnerGlob
	}
	if opts.CivmctlPath != "" {
		hookOpts.CivmctlPath = opts.CivmctlPath
	}
	res := opts.HookInstallFn(ctx, hookOpts)
	event := WatchdogEvent{
		Event:    "hooks-repaired",
		Severity: "info",
		Executed: opts.Execute,
		Detail:   fmt.Sprintf("%d runner env file(s)", len(res.RunnerEnvFiles)),
	}
	if res.Error != "" {
		event.Severity = "critical"
		event.Reason = "hook-install-failed"
		event.Detail = res.Error
		report.add(event)
		return errors.New(res.Error)
	}
	report.add(event)
	return nil
}

func restartWatchdogRunners(ctx context.Context, opts WatchdogOptions, systemd []Status, ghByRepo map[string][]WatchdogGitHubRunner, report *WatchdogReport) error {
	candidates := restartCandidates(systemd, ghByRepo)
	// Serialização (#1184): NUNCA ressuscitar um runner por-repo que colide com
	// um runner org da mesma org. Sem isto, um runner por-repo só disabled (mas
	// loaded) aparece inactive/dead em restartCandidates e seria reativado aqui,
	// reabrindo a janela de 2 jobs concorrentes no mesmo disco. O estado durável
	// é a remoção do runner por-repo (serialize-runner.ps1 / civmctl runner
	// remove); até lá, o watchdog declina o restart e registra o evento.
	for _, c := range DetectCollisions(systemd) {
		if _, isCandidate := candidates[c.RepoUnit]; isCandidate {
			delete(candidates, c.RepoUnit)
			report.add(WatchdogEvent{
				Event: "runner-restart-skipped", Severity: "warning", Unit: c.RepoUnit,
				Reason: "redundant-repo-runner",
				Detail: fmt.Sprintf("colide com %s para %s; remover em vez de restartar", c.OrgName, c.Repo),
			})
		}
	}
	for _, unit := range sortedMapKeys(candidates) {
		reason := candidates[unit]
		event := WatchdogEvent{Event: "runner-restarted", Severity: "info", Unit: unit, Reason: reason, Executed: opts.Execute}
		if !opts.Execute {
			report.add(event)
			continue
		}
		if hasLocalRunnerWorker(ctx, opts) {
			report.add(WatchdogEvent{
				Event: "runner-restart-skipped", Severity: "warning", Unit: unit,
				Reason: "host-busy", Detail: "Runner.Worker appeared before restart",
			})
			continue
		}
		if err := opts.RestartFn(ctx, unit); err != nil {
			if errors.Is(err, errWatchdogRunnerBecameBusy) {
				report.add(WatchdogEvent{
					Event: "runner-restart-skipped", Severity: "warning", Unit: unit,
					Reason: "host-busy-after-freeze", Detail: err.Error(),
				})
				continue
			}
			event.Severity = "critical"
			event.Detail = err.Error()
			report.add(event)
			return err
		}
		report.add(event)
	}
	return nil
}

func recoverWatchdogOrgBusyWithoutWorker(ctx context.Context, opts WatchdogOptions, systemd []Status, report *WatchdogReport) {
	if hasLocalRunnerWorker(ctx, opts) {
		clearPendingWatchdogOrgBusyDwell(opts, systemd, report)
		return
	}
	opts.SleepFn(opts.IdleProbeDelay)
	if hasLocalRunnerWorker(ctx, opts) {
		clearPendingWatchdogOrgBusyDwell(opts, systemd, report)
		return
	}
	for _, local := range systemd {
		if local.UnitName == "" || local.Org == "" || local.ActiveState != "active" || local.SubState != "running" {
			continue
		}
		runners, err := listWatchdogGitHubOrgRunners(ctx, local.Org, opts.RunFn)
		if err != nil {
			report.add(WatchdogEvent{
				Event: "runner-online-unknown", Severity: "warning", Unit: local.UnitName,
				Reason: "github-org-runners-failed", Detail: err.Error(),
			})
			continue
		}
		for _, gh := range runners {
			if gh.Name != local.Name || gh.Status != "online" || !gh.Busy {
				continue
			}
			event := WatchdogEvent{
				Event: "runner-restarted", Severity: "info", Unit: local.UnitName,
				Runner: gh.Name, Reason: "github-busy-without-local-worker", Executed: opts.Execute,
			}
			state, err := loadRerunState(opts)
			if err != nil {
				report.add(WatchdogEvent{
					Event: "runner-restart-skipped", Severity: "critical", Unit: local.UnitName,
					Runner: gh.Name, Reason: "marker-read-failed", Detail: err.Error(),
				})
				report.Exit = maxExit(report.Exit, 2)
				return
			}
			now := opts.NowFn()
			key := "org-busy:" + local.UnitName
			window := state.AutoRestarts[key]
			if window.WindowStart.IsZero() ||
				(window.Count >= 1 && now.Sub(window.WindowStart) >= time.Hour) {
				window = autoRestartWindow{WindowStart: now}
			}
			if window.Count == 0 && opts.QueueStallDwell > 0 {
				age := now.Sub(window.WindowStart)
				if age < 0 {
					window.WindowStart = now
					age = 0
				}
				state.AutoRestarts[key] = window
				if opts.Execute {
					if err := writeRerunState(opts, state); err != nil {
						report.add(WatchdogEvent{
							Event: "runner-restart-skipped", Severity: "critical", Unit: local.UnitName,
							Runner: gh.Name, Reason: "marker-write-failed", Detail: err.Error(),
						})
						report.Exit = maxExit(report.Exit, 2)
						return
					}
				}
				if age < opts.QueueStallDwell {
					report.add(WatchdogEvent{
						Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
						Runner: gh.Name, Reason: "org-busy-dwell", Executed: opts.Execute,
						AgeSeconds: int64(age.Seconds()),
					})
					return
				}
			}
			if window.Count >= 1 {
				report.add(WatchdogEvent{
					Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
					Runner: gh.Name, Reason: "org-busy-cooldown",
					Detail: "already restarted once in the rolling hour",
				})
				return
			}
			if !opts.Execute {
				report.add(event)
				return
			}
			if hasLocalRunnerWorker(ctx, opts) {
				report.add(WatchdogEvent{
					Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
					Runner: gh.Name, Reason: "host-busy", Detail: "Runner.Worker appeared before restart",
				})
				return
			}
			window.Count++
			window.LastActed = now
			state.AutoRestarts[key] = window
			if err := writeRerunState(opts, state); err != nil {
				report.add(WatchdogEvent{
					Event: "runner-restart-skipped", Severity: "critical", Unit: local.UnitName,
					Runner: gh.Name, Reason: "marker-write-failed", Detail: err.Error(),
				})
				report.Exit = maxExit(report.Exit, 2)
				return
			}
			if err := opts.RestartFn(ctx, local.UnitName); err != nil {
				if errors.Is(err, errWatchdogRunnerBecameBusy) {
					report.add(WatchdogEvent{
						Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
						Runner: gh.Name, Reason: "host-busy-after-freeze", Detail: err.Error(),
					})
					return
				}
				event.Severity = "critical"
				event.Detail = err.Error()
				report.add(event)
				report.Exit = maxExit(report.Exit, 2)
				return
			}
			report.add(event)
			return
		}
		clearPendingWatchdogOrgBusyDwell(opts, []Status{local}, report)
	}
}

func clearPendingWatchdogOrgBusyDwell(opts WatchdogOptions, systemd []Status, report *WatchdogReport) {
	if !opts.Execute {
		return
	}
	state, err := loadRerunState(opts)
	if err != nil {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "critical",
			Reason: "marker-read-failed", Detail: err.Error(),
		})
		report.Exit = maxExit(report.Exit, 2)
		return
	}
	changed := false
	for _, local := range systemd {
		if local.UnitName == "" || local.Org == "" {
			continue
		}
		key := "org-busy:" + local.UnitName
		if window, ok := state.AutoRestarts[key]; ok && window.Count == 0 {
			delete(state.AutoRestarts, key)
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := writeRerunState(opts, state); err != nil {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "critical",
			Reason: "marker-write-failed", Detail: err.Error(),
		})
		report.Exit = maxExit(report.Exit, 2)
	}
}

func recoverWatchdogOrgIdleWithQueue(
	ctx context.Context,
	opts WatchdogOptions,
	systemd []Status,
	repos []string,
	report *WatchdogReport,
) {
	var local *Status
	for i := range systemd {
		status := &systemd[i]
		if status.UnitName == "" || status.Org == "" ||
			status.ActiveState != "active" || status.SubState != "running" {
			continue
		}
		if local != nil {
			report.add(WatchdogEvent{
				Event: "runner-restart-skipped", Severity: "warning",
				Reason: "ambiguous-org-runner",
				Detail: "mais de 1 runner org ativo; nenhuma unit escolhida",
			})
			return
		}
		local = status
	}
	if local == nil {
		return
	}

	if len(repos) == 0 {
		return
	}

	ghRunner, found, err := watchdogOrgRunner(ctx, opts, *local)
	if err != nil {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
			Reason: "github-org-runners-failed", Detail: err.Error(),
		})
		report.Exit = maxExit(report.Exit, 1)
		return
	}
	if !found {
		return
	}

	state, err := loadRerunState(opts)
	if err != nil {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "critical", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "marker-read-failed", Detail: err.Error(),
		})
		report.Exit = maxExit(report.Exit, 2)
		return
	}
	incident, exists := state.QueueStalls[local.UnitName]
	if ghRunner.Status == "online" && ghRunner.Busy {
		clearQueueStallIncident(
			opts,
			state,
			*local,
			ghRunner,
			exists,
			"runner-consuming-job",
			report,
		)
		return
	}
	if ghRunner.Status != "online" || ghRunner.Busy {
		return
	}

	now := opts.NowFn()
	evidence, err := opts.QueuedCivmJobsFn(ctx, repos, now, opts.MaxRunAge)
	if err != nil {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "queued-jobs-unknown", Detail: err.Error(),
		})
		report.Exit = maxExit(report.Exit, 1)
		return
	}
	if evidence.EligibleJobs == 0 {
		clearQueueStallIncident(opts, state, *local, ghRunner, exists, "eligible-queue-empty", report)
		return
	}

	idleResult := idle.Check(ctx, idle.Options{
		ActivityFn: opts.ActivityFn,
		ProbeDelay: opts.IdleProbeDelay,
	})
	if idleResult.Status != idle.StatusIdle {
		reason := "host-busy"
		detail := idle.FormatActivities(idleResult.Activities)
		if idleResult.Status == idle.StatusUnknown {
			reason = "host-idle-unknown"
			detail = idleResult.Error
		}
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: reason, Detail: detail,
			QueuedJobs: evidence.EligibleJobs,
		})
		return
	}

	report.Metrics.QueueStalls++
	if !exists || incident.Signature != evidence.Signature {
		incident = queueStallIncident{Signature: evidence.Signature, FirstSeen: now}
		if opts.Execute {
			state.QueueStalls[local.UnitName] = incident
			if err := writeRerunState(opts, state); err != nil {
				report.add(WatchdogEvent{
					Event: "runner-restart-skipped", Severity: "critical", Unit: local.UnitName,
					Runner: ghRunner.Name, Reason: "marker-write-failed", Detail: err.Error(),
				})
				report.Exit = maxExit(report.Exit, 2)
				return
			}
		}
		report.add(WatchdogEvent{
			Event: "queue-stall-armed", Severity: "warning", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "github-online-idle-with-civm-queue",
			QueuedJobs: evidence.EligibleJobs, Executed: opts.Execute,
		})
		return
	}

	age := now.Sub(incident.FirstSeen)
	if !incident.RestartedAt.IsZero() {
		report.Metrics.StallsUnresolved++
		report.add(WatchdogEvent{
			Event: "queue-stall-unresolved", Severity: "critical", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "restart-did-not-advance-queue",
			QueuedJobs: evidence.EligibleJobs, AgeSeconds: int64(age.Seconds()),
		})
		report.Exit = maxExit(report.Exit, 2)
		return
	}
	if age < opts.QueueStallDwell {
		report.add(WatchdogEvent{
			Event: "queue-stall-observed", Severity: "warning", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "dwell-not-reached",
			QueuedJobs: evidence.EligibleJobs, AgeSeconds: int64(age.Seconds()),
		})
		return
	}
	if !opts.Execute {
		report.add(WatchdogEvent{
			Event: "runner-restarted", Severity: "info", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "github-online-idle-with-civm-queue",
			QueuedJobs: evidence.EligibleJobs, AgeSeconds: int64(age.Seconds()),
		})
		return
	}

	// Revalidate all three signals immediately before persisting the incident
	// and mutating: GitHub session, eligible queue, and the full idle check.
	if _, ready, err := watchdogOnlineIdleOrgRunner(ctx, opts, *local); err != nil || !ready {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "runner-state-changed", Detail: detail,
		})
		return
	}
	revalidated, err := opts.QueuedCivmJobsFn(ctx, repos, now, opts.MaxRunAge)
	if err != nil || revalidated.EligibleJobs == 0 ||
		revalidated.Signature != evidence.Signature {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "queue-state-changed", Detail: detail,
		})
		return
	}
	idleResult = idle.Check(ctx, idle.Options{
		ActivityFn: opts.ActivityFn,
		ProbeDelay: opts.IdleProbeDelay,
	})
	if idleResult.Status != idle.StatusIdle {
		reason := "host-busy"
		if idleResult.Status == idle.StatusUnknown {
			reason = "host-idle-unknown"
		}
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: reason,
		})
		return
	}

	incident.RestartedAt = now
	state.QueueStalls[local.UnitName] = incident
	if err := writeRerunState(opts, state); err != nil {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "critical", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "marker-write-failed", Detail: err.Error(),
		})
		report.Exit = maxExit(report.Exit, 2)
		return
	}

	event := WatchdogEvent{
		Event: "runner-restarted", Severity: "info", Unit: local.UnitName,
		Runner: ghRunner.Name, Reason: "github-online-idle-with-civm-queue",
		QueuedJobs: evidence.EligibleJobs, AgeSeconds: int64(age.Seconds()),
		Executed: true,
	}
	if err := opts.RestartFn(ctx, local.UnitName); err != nil {
		if errors.Is(err, errWatchdogRunnerBecameBusy) {
			incident.RestartedAt = time.Time{}
			state.QueueStalls[local.UnitName] = incident
			if writeErr := writeRerunState(opts, state); writeErr != nil {
				report.add(WatchdogEvent{
					Event: "runner-restart-skipped", Severity: "critical", Unit: local.UnitName,
					Runner: ghRunner.Name, Reason: "marker-write-failed", Detail: writeErr.Error(),
				})
				report.Exit = maxExit(report.Exit, 2)
				return
			}
			report.add(WatchdogEvent{
				Event: "runner-restart-skipped", Severity: "warning", Unit: local.UnitName,
				Runner: ghRunner.Name, Reason: "host-busy-after-freeze", Detail: err.Error(),
				QueuedJobs: evidence.EligibleJobs,
			})
			return
		}
		event.Severity = "critical"
		event.Detail = err.Error()
		report.Exit = maxExit(report.Exit, 2)
	}
	report.Metrics.RunnerRestarts++
	report.add(event)
}

func hasWatchdogOrgRunner(systemd []Status) bool {
	for _, status := range systemd {
		if status.Org != "" {
			return true
		}
	}
	return false
}

func normalizeWatchdogRepos(repos []string) ([]string, error) {
	seen := make(map[string]struct{}, len(repos))
	normalized := make([]string, 0, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if err := civm.ValidateRepo(repo); err != nil {
			return nil, fmt.Errorf("fleet do watchdog: %w", err)
		}
		if _, duplicate := seen[repo]; duplicate {
			continue
		}
		seen[repo] = struct{}{}
		normalized = append(normalized, repo)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func clearQueueStallIncident(
	opts WatchdogOptions,
	state rerunState,
	local Status,
	ghRunner WatchdogGitHubRunner,
	exists bool,
	reason string,
	report *WatchdogReport,
) {
	if !exists {
		return
	}
	event := WatchdogEvent{
		Event: "queue-stall-recovered", Severity: "info", Unit: local.UnitName,
		Runner: ghRunner.Name, Reason: reason, Executed: opts.Execute,
	}
	if !opts.Execute {
		report.add(event)
		return
	}
	delete(state.QueueStalls, local.UnitName)
	if err := writeRerunState(opts, state); err != nil {
		report.add(WatchdogEvent{
			Event: "runner-restart-skipped", Severity: "critical", Unit: local.UnitName,
			Runner: ghRunner.Name, Reason: "marker-write-failed", Detail: err.Error(),
		})
		report.Exit = maxExit(report.Exit, 2)
		return
	}
	report.add(event)
}

func watchdogOnlineIdleOrgRunner(
	ctx context.Context,
	opts WatchdogOptions,
	local Status,
) (WatchdogGitHubRunner, bool, error) {
	gh, found, err := watchdogOrgRunner(ctx, opts, local)
	if err != nil || !found {
		return WatchdogGitHubRunner{}, false, err
	}
	if gh.Status == "online" && !gh.Busy {
		return gh, true, nil
	}
	return WatchdogGitHubRunner{}, false, nil
}

func watchdogOrgRunner(
	ctx context.Context,
	opts WatchdogOptions,
	local Status,
) (WatchdogGitHubRunner, bool, error) {
	runners, err := listWatchdogGitHubOrgRunners(ctx, local.Org, opts.RunFn)
	if err != nil {
		return WatchdogGitHubRunner{}, false, err
	}
	for _, gh := range runners {
		if gh.Name == local.Name &&
			hasWatchdogLabel(gh.Labels, "self-hosted") &&
			hasWatchdogLabel(gh.Labels, "civm") {
			return gh, true, nil
		}
	}
	return WatchdogGitHubRunner{}, false, nil
}

func hasLocalRunnerWorker(ctx context.Context, opts WatchdogOptions) bool {
	activities, err := opts.ActivityFn(ctx)
	if err != nil {
		return true
	}
	for _, activity := range activities {
		if strings.Contains(activity.Command, "Runner.Worker") {
			return true
		}
	}
	return false
}

// hookLogRecord is the subset of a hooks.jsonl line the watchdog needs to detect
// a broken-runner sentinel and map it to a runner unit.
type hookLogRecord struct {
	Time     time.Time `json:"time"`
	Decision string    `json:"decision"`
	WorkRoot string    `json:"work_root"`
	Actions  []struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	} `json:"actions"`
}

func hookRecordIsBrokenSentinel(rec hookLogRecord) bool {
	// The wedging signal is specifically a work_root cleanup that failed: the
	// privileged escalation could not reclaim a root-owned _work leftover, so the
	// runner is stuck at "Complete runner" and every next job fails. We do NOT
	// trigger on decision=="error" alone (it also covers disk-pressure rejections
	// that need no restart) — only on the work_root action error (DT-8).
	for _, a := range rec.Actions {
		if a.Name == "work_root" && strings.TrimSpace(a.Error) != "" {
			return true
		}
	}
	return false
}

// unitForWorkRoot maps a hook work_root to the owning runner unit deterministically
// via Status.WorkingDirectory (work_root = <WorkingDirectory>/_work[...]). Longest
// matching prefix wins so a shorter dir never shadows a nested one. Returns "" when
// no unit owns the path — the watchdog then refuses to restart anything (never a
// guess on a shared box).
func unitForWorkRoot(workRoot string, systemd []Status) string {
	clean := filepath.Clean(workRoot)
	best, bestLen := "", -1
	for _, s := range systemd {
		if s.UnitName == "" || s.WorkingDirectory == "" {
			continue
		}
		wd := filepath.Clean(s.WorkingDirectory)
		if clean == wd || strings.HasPrefix(clean, wd+string(filepath.Separator)) {
			if len(wd) > bestLen {
				best, bestLen = s.UnitName, len(wd)
			}
		}
	}
	return best
}

// detectBrokenRunner scans the tail of the shared hooks.jsonl for a broken-runner
// sentinel (a recent work_root cleanup error => wedged runner), maps it to the
// owning systemd unit via WorkingDirectory, and restarts that unit, capped per
// unit per rolling hour (anti restart-loop). It runs BEFORE the idle skip and is
// NOT gated by host-idle: a wedged unit must recover even while other units are
// busy — restarting one unit does not disturb another unit's job. Best-effort:
// an unreadable/absent/truncated log is a no-op, never a watchdog failure
// (RF-6 / ITEM-10 / DT-8).
func detectBrokenRunner(ctx context.Context, opts WatchdogOptions, systemd []Status, report *WatchdogReport) {
	now := opts.NowFn()
	// Pipeline em 4 estágios: lê os sentinels do log → mapeia cada work_root para
	// a unit dona → escolhe quais units restartar (respeitando cap + dedup, e
	// persistindo o estado ANTES de agir) → executa os restarts. Cada estágio é
	// um no-op silencioso quando não há nada a fazer (best-effort, RF-6/ITEM-10).
	sentinelAt := parseBrokenSentinels(opts, now)
	if len(sentinelAt) == 0 {
		return
	}
	unitSentinel := mapSentinelsToUnits(sentinelAt, systemd, report)
	if len(unitSentinel) == 0 {
		return
	}
	state, err := loadRerunState(opts)
	if err != nil {
		report.add(WatchdogEvent{Event: "runner-auto-restart-skipped", Severity: "warning", Reason: "marker-read-failed", Detail: err.Error()})
		return
	}
	toRestart := selectUnitsToRestart(opts, now, unitSentinel, &state, report)
	if len(toRestart) == 0 {
		return
	}
	// F1 fail-closed: persist the reserved cap/dedup state BEFORE restarting. If
	// the marker cannot be written, abort without restarting — a missed recovery
	// is safer than an uncapped restart loop (a write failure is correlated with
	// the disk/permission fault that wedged the runner in the first place).
	if err := writeRerunState(opts, state); err != nil {
		report.add(WatchdogEvent{Event: "runner-auto-restart-skipped", Severity: "critical", Reason: "marker-write-failed", Detail: err.Error()})
		report.Exit = maxExit(report.Exit, 2)
		return
	}
	executeRestarts(ctx, opts, toRestart, report)
}

// parseBrokenSentinels lê o tail do hooks.jsonl compartilhado e devolve, por
// work_root, o timestamp do sentinel de broken-runner mais recente dentro da
// janela de 1h. Um log ausente/ilegível/truncado, ou sem sentinels na janela,
// resulta num mapa vazio — nunca um erro (best-effort, DT-8).
func parseBrokenSentinels(opts WatchdogOptions, now time.Time) map[string]time.Time {
	data, err := opts.ReadFileFn(opts.HooksLogPath)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	cutoff := now.Add(-time.Hour)
	lines := strings.Split(string(data), "\n")
	if len(lines) > 500 {
		lines = lines[len(lines)-500:]
	}
	// Newest in-window sentinel timestamp per work_root.
	sentinelAt := map[string]time.Time{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec hookLogRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue // skip malformed/truncated lines
		}
		if rec.WorkRoot == "" || !hookRecordIsBrokenSentinel(rec) {
			continue
		}
		ts := rec.Time
		if ts.IsZero() {
			ts = now
		}
		if ts.Before(cutoff) {
			continue // stale sentinel
		}
		if ts.After(sentinelAt[rec.WorkRoot]) {
			sentinelAt[rec.WorkRoot] = ts
		}
	}
	return sentinelAt
}

// mapSentinelsToUnits resolve deterministicamente cada work_root para a unit
// dona (via WorkingDirectory) e devolve, por unit, o timestamp do sentinel mais
// recente. work_roots sem unit dona viram um skip "no-unit-for-work-root" no
// report (nunca um chute num box compartilhado).
func mapSentinelsToUnits(sentinelAt map[string]time.Time, systemd []Status, report *WatchdogReport) map[string]time.Time {
	unitSentinel := map[string]time.Time{}
	for workRoot, ts := range sentinelAt {
		unit := unitForWorkRoot(workRoot, systemd)
		if unit == "" {
			report.add(WatchdogEvent{Event: "runner-auto-restart-skipped", Severity: "warning", Reason: "no-unit-for-work-root", Detail: workRoot})
			continue
		}
		if ts.After(unitSentinel[unit]) {
			unitSentinel[unit] = ts
		}
	}
	return unitSentinel
}

// selectUnitsToRestart percorre as units (em ordem estável) aplicando o cap por
// hora e o dedup F2, e reserva o slot no `state` em memória para cada unit que
// vai de fato restartar. O caller é quem PERSISTE `state` antes de chamar
// executeRestarts (invariante F1 persist-before-restart). Em dry-run apenas
// surfaceia o candidato e nunca acrescenta à lista de restart.
func selectUnitsToRestart(opts WatchdogOptions, now time.Time, unitSentinel map[string]time.Time, state *rerunState, report *WatchdogReport) []string {
	units := make([]string, 0, len(unitSentinel))
	for u := range unitSentinel {
		units = append(units, u)
	}
	sort.Strings(units)

	var toRestart []string
	for _, unit := range units {
		ts := unitSentinel[unit]
		w := state.AutoRestarts[unit]
		if w.WindowStart.IsZero() || now.Sub(w.WindowStart) >= time.Hour {
			// Reset the hourly Count window but KEEP the dedup cursor.
			w = autoRestartWindow{WindowStart: now, LastActed: w.LastActed}
		}
		// F2 dedup: never act twice on the same (or an older) sentinel. The line
		// stays in hooks.jsonl after we restart, so without this a single
		// already-resolved incident would restart a healthy runner every tick up
		// to the cap.
		if !ts.After(w.LastActed) {
			continue
		}
		if w.Count >= opts.AutoRestartPerHour {
			report.add(WatchdogEvent{Event: "runner-auto-restart-skipped", Severity: "warning", Unit: unit, Reason: "rate-cap-reached", Detail: fmt.Sprintf("%d/%d this hour", w.Count, opts.AutoRestartPerHour)})
			continue
		}
		if !opts.Execute {
			report.add(WatchdogEvent{Event: "runner-auto-restarted", Severity: "warning", Unit: unit, Reason: "broken-runner-sentinel", Executed: false})
			continue
		}
		// Reserve the slot in memory; it is PERSISTED before any restart below.
		w.Count++
		w.LastActed = ts
		state.AutoRestarts[unit] = w
		toRestart = append(toRestart, unit)
	}
	return toRestart
}

// executeRestarts restarta cada unit já reservada (e persistida) via RestartFn.
// Uma falha de restart vira evento crítico + exit 2, mas não aborta as demais —
// cada unit é independente no box compartilhado.
func executeRestarts(ctx context.Context, opts WatchdogOptions, toRestart []string, report *WatchdogReport) {
	for _, unit := range toRestart {
		event := WatchdogEvent{Event: "runner-auto-restarted", Severity: "warning", Unit: unit, Reason: "broken-runner-sentinel", Executed: true}
		if err := opts.RestartFn(ctx, unit); err != nil {
			if errors.Is(err, errWatchdogRunnerBecameBusy) {
				report.add(WatchdogEvent{
					Event: "runner-auto-restart-skipped", Severity: "warning", Unit: unit,
					Reason: "host-busy-after-freeze", Detail: err.Error(),
				})
				continue
			}
			event.Severity = "critical"
			event.Detail = err.Error()
			report.Exit = maxExit(report.Exit, 2)
		}
		report.add(event)
	}
}

// defaultWatchdogRestart closes the last idle-check/restart TOCTOU window by
// freezing an active unit before its final Worker probe. The listener cannot
// accept a new job while frozen; every skip/error thaws it. Inactive/failed
// units cannot accept work and may be restarted without a freeze.
func defaultWatchdogRestart(ctx context.Context, opts WatchdogOptions, unit string) (retErr error) {
	if err := civm.ValidateServiceUnit(unit); err != nil {
		return err
	}
	frozen := false
	if _, err := opts.RunFn(ctx, "sudo", "systemctl", "freeze", unit); err != nil {
		out, _ := opts.RunFn(ctx, "systemctl", "is-active", unit)
		state := strings.TrimSpace(string(out))
		if state != "inactive" && state != "failed" {
			return fmt.Errorf("systemctl freeze %s while state=%q: %w", unit, state, err)
		}
	} else {
		frozen = true
	}
	defer func() {
		if !frozen {
			return
		}
		if _, err := opts.RunFn(ctx, "sudo", "systemctl", "thaw", unit); err != nil && retErr == nil {
			retErr = fmt.Errorf("systemctl thaw %s: %w", unit, err)
		}
	}()
	if frozen && hasLocalRunnerWorker(ctx, opts) {
		return errWatchdogRunnerBecameBusy
	}
	// Actions Runner units are generated with KillMode=process. After an OOM,
	// runsvc.sh may be dead while RunnerService.js/Runner.Listener stay alive in
	// the unit cgroup; a plain restart then creates a second listener that loops
	// on broker session conflict. Stop first so a healthy listener can close its
	// broker session, then purge only residual cgroup processes. The final Worker
	// probe above makes both mutations safe.
	if _, err := opts.RunFn(ctx, "sudo", "systemctl", "stop", unit); err != nil {
		return fmt.Errorf("systemctl stop %s: %w", unit, err)
	}
	// A successful stop necessarily releases the freeze. Avoid a thaw against an
	// inactive unit in the deferred cleanup path.
	frozen = false
	if _, err := opts.RunFn(ctx, "sudo", "systemctl", "kill", "--kill-who=all", "--signal=SIGKILL", unit); err != nil {
		empty, proofErr := watchdogUnitCgroupEmpty(ctx, opts, unit)
		if proofErr != nil {
			return fmt.Errorf("systemctl kill %s cgroup and empty proof: %w", unit, errors.Join(err, proofErr))
		}
		if !empty {
			return fmt.Errorf("systemctl kill %s cgroup: %w (residual PIDs remain)", unit, err)
		}
	}
	if _, err := opts.RunFn(ctx, "sudo", "systemctl", "restart", unit); err != nil {
		return fmt.Errorf("systemctl restart %s: %w", unit, err)
	}
	if frozen {
		if _, err := opts.RunFn(ctx, "sudo", "systemctl", "thaw", unit); err != nil {
			return fmt.Errorf("systemctl thaw %s after restart: %w", unit, err)
		}
		frozen = false
	}
	opts.SleepFn(opts.RestartDelay)
	out, _ := opts.RunFn(ctx, "systemctl", "is-active", unit)
	if active := strings.TrimSpace(string(out)); active != "active" {
		return fmt.Errorf("%s is-active=%q after restart", unit, active)
	}
	return nil
}

func watchdogUnitCgroupEmpty(ctx context.Context, opts WatchdogOptions, unit string) (bool, error) {
	out, err := opts.RunFn(ctx, "systemctl", "show", unit, "--property=ControlGroup", "--value")
	if err != nil {
		return false, err
	}
	group := strings.TrimSpace(string(out))
	if group == "" {
		return true, nil
	}
	clean := filepath.Clean(group)
	if !strings.HasPrefix(clean, "/system.slice/") {
		return false, fmt.Errorf("unsafe control group %q", group)
	}
	procsPath := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(clean, "/"), "cgroup.procs")
	data, err := opts.ReadFileFn(procsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(data)) == "", nil
}

func rerunWatchdogNetworkFailures(ctx context.Context, opts WatchdogOptions, repos []string, repoOnline map[string]bool, report *WatchdogReport) error {
	state, err := loadRerunState(opts)
	if err != nil {
		report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "critical", Reason: "marker-read-failed", Detail: err.Error()})
		return err
	}
	mutated := false
	for _, repo := range repos {
		if !repoOnline[repo] {
			report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "warning", Repo: repo, Reason: "runner-not-online"})
			continue
		}
		runs, err := opts.ListRunsFn(ctx, repo, opts.RunLimit)
		if err != nil {
			report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "warning", Repo: repo, Reason: "run-list-failed", Detail: err.Error()})
			report.Exit = maxExit(report.Exit, 1)
			continue
		}
		for _, run := range runs {
			report.Metrics.RunsConsidered++
			// Um erro fatal de rerun (gh run rerun falhou) aborta toda a função, igual
			// ao comportamento anterior do `return err` inline no loop.
			if err := evaluateNetworkRerun(ctx, opts, repo, run, &state, &mutated, report); err != nil {
				return err
			}
		}
	}
	if mutated {
		if err := writeRerunState(opts, state); err != nil {
			report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "critical", Reason: "marker-write-failed", Detail: err.Error()})
			return err
		}
	}
	return nil
}

// evaluateNetworkRerun processa um único workflow run: filtra runs não elegíveis,
// busca o PR aberto e o log, classifica a falha e — só quando é network-checkout
// e Execute está ligado — dispara o rerun e grava o marker de dedup. Devolve um
// erro NÃO-nil apenas no caso fatal de `RerunFn` falhar (o caller aborta a função
// inteira); todos os outros caminhos são skips registrados no report e retornam
// nil para o run seguinte. Pode setar `*mutated=true` quando grava um marker novo.
func evaluateNetworkRerun(ctx context.Context, opts WatchdogOptions, repo string, run WatchdogRun, state *rerunState, mutated *bool, report *WatchdogReport) error {
	if reason := skipRunBeforeLog(opts, repo, run, *state); reason != "" {
		report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "info", Repo: repo, RunID: run.ID, HeadSHA: run.HeadSHA, Reason: reason})
		return nil
	}
	pr, reason, err := openPullRequestForRun(ctx, opts, repo, run)
	if err != nil {
		report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "warning", Repo: repo, RunID: run.ID, HeadSHA: run.HeadSHA, Reason: "pr-check-failed", Detail: err.Error()})
		report.Exit = maxExit(report.Exit, 1)
		return nil
	}
	if reason != "" {
		report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "info", Repo: repo, RunID: run.ID, HeadSHA: run.HeadSHA, Reason: reason})
		return nil
	}
	log, err := opts.RunLogFn(ctx, repo, run.ID)
	if err != nil {
		report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "warning", Repo: repo, RunID: run.ID, HeadSHA: run.HeadSHA, Reason: "log-fetch-failed", Detail: err.Error()})
		report.Exit = maxExit(report.Exit, 1)
		return nil
	}
	classification := ClassifyFailureLog(log)
	if classification.Kind != FailureNetworkCheckout {
		report.add(WatchdogEvent{Event: "rerun-skipped", Severity: "info", Repo: repo, RunID: run.ID, HeadSHA: run.HeadSHA, Reason: string(classification.Kind), Detail: classification.Detail})
		return nil
	}
	event := WatchdogEvent{
		Event:    "rerun-triggered",
		Severity: "info",
		Repo:     repo,
		RunID:    run.ID,
		HeadSHA:  run.HeadSHA,
		Reason:   "network-checkout",
		Detail:   fmt.Sprintf("pr=%d signature=%s", pr.Number, classification.Signature),
		Executed: opts.Execute,
	}
	if opts.Execute {
		if err := opts.RerunFn(ctx, repo, run.ID); err != nil {
			event.Severity = "critical"
			event.Detail = fmt.Sprintf("gh run rerun: %v", err)
			report.add(event)
			return err
		}
		state.Reruns[rerunMarkerKey(run.ID, run.HeadSHA)] = RerunMarker{
			Repo:    repo,
			RunID:   run.ID,
			HeadSHA: run.HeadSHA,
			RerunAt: opts.NowFn().UTC(),
		}
		*mutated = true
	}
	report.add(event)
	return nil
}

func skipRunBeforeLog(opts WatchdogOptions, repo string, run WatchdogRun, state rerunState) string {
	if err := civm.ValidateRepo(repo); err != nil {
		return "repo-invalid"
	}
	if !rerunnableConclusion(run.Conclusion) {
		return "conclusion-not-rerunnable"
	}
	if run.CreatedAt.IsZero() {
		return "run-created-at-missing"
	}
	if opts.NowFn().Sub(run.CreatedAt) > opts.MaxRunAge {
		return "run-too-old"
	}
	if len(run.PullRequests) == 0 {
		return "no-open-pr-association"
	}
	if state.Reruns[rerunMarkerKey(run.ID, run.HeadSHA)].RunID != 0 {
		return "already-rerun"
	}
	return ""
}

func openPullRequestForRun(ctx context.Context, opts WatchdogOptions, repo string, run WatchdogRun) (WatchdogPullRequest, string, error) {
	lastReason := "no-open-pr-association"
	for _, ref := range run.PullRequests {
		if ref.Number <= 0 {
			continue
		}
		pr, err := opts.PullRequestFn(ctx, repo, ref.Number)
		if err != nil {
			return WatchdogPullRequest{}, "", err
		}
		if pr.State != "open" {
			lastReason = "pr-not-open"
			continue
		}
		mergeable := strings.ToLower(strings.TrimSpace(pr.MergeableState))
		if mergeable == "dirty" || mergeable == "conflicting" {
			return pr, "pr-conflicting", nil
		}
		return pr, "", nil
	}
	return WatchdogPullRequest{}, lastReason, nil
}

func ClassifyFailureLog(log string) FailureClassification {
	lower := strings.ToLower(log)
	if sig, idx := firstSignature(lower, secretSignatures); idx >= 0 {
		return FailureClassification{Kind: FailureSecret, Signature: sig, Detail: "secret/auth signature present"}
	}
	sig, networkIdx := firstSignature(lower, networkCheckoutSignatures)
	if actionSig, actionIdx := firstSignature(lower, actionDownloadTransientSignatures); actionIdx >= 0 && (networkIdx < 0 || actionIdx < networkIdx) {
		sig, networkIdx = actionSig, actionIdx
	}
	if networkIdx < 0 {
		if codeSig, idx := firstSignature(lower, codeFailureSignatures); idx >= 0 {
			return FailureClassification{Kind: FailureCode, Signature: codeSig, Detail: "code/test/lint/build signature present"}
		}
		return FailureClassification{Kind: FailureUnknown, Detail: "no network checkout signature"}
	}
	if codeSig, codeIdx := firstSignature(lower, codeFailureSignatures); codeIdx >= 0 && codeIdx < networkIdx {
		return FailureClassification{Kind: FailureCode, Signature: codeSig, Detail: "code/test/lint/build step started before network failure"}
	}
	return FailureClassification{Kind: FailureNetworkCheckout, Signature: sig, Detail: "transient checkout/network signature"}
}

var actionDownloadTransientSignatures = []string{
	"failed to resolve action download info. error: internal server error",
	"failed to resolve action download info. error: service unavailable",
}

var networkCheckoutSignatures = []string{
	"rpc failed",
	"early eof",
	"invalid index-pack",
	"curl 56",
	"curl 92",
	"gnutls recv error",
	"connection timed out",
	"http/2 cancel",
	"http/2 stream",
	"the remote end hung up unexpectedly",
}

var codeFailureSignatures = []string{
	"run go test",
	"run npm test",
	"run pnpm test",
	"run yarn test",
	"run make test",
	"run go build",
	"run npm run build",
	"run pnpm build",
	"run yarn build",
	"run golangci-lint",
	"run eslint",
	"run npm run lint",
	"run pnpm lint",
	"run yarn lint",
	"run tsc",
	"fail\t",
	"npm err!",
	"eslint",
	"test failed",
	"tests failed",
	"build failed",
	"lint failed",
}

var secretSignatures = []string{
	"bad credentials",
	"authentication failed",
	"could not read username",
	"resource not accessible by integration",
	"permission denied (publickey)",
	"a secret with the name",
	"secret not found",
	"secrets are not passed",
}

func firstSignature(haystack string, signatures []string) (string, int) {
	bestSig := ""
	bestIdx := -1
	for _, sig := range signatures {
		idx := strings.Index(haystack, sig)
		if idx == -1 {
			continue
		}
		if bestIdx == -1 || idx < bestIdx {
			bestIdx = idx
			bestSig = sig
		}
	}
	return bestSig, bestIdx
}

func collectWatchdogGitHubRunners(ctx context.Context, opts WatchdogOptions, repos []string, report *WatchdogReport) (map[string][]WatchdogGitHubRunner, map[string]bool) {
	byRepo := map[string][]WatchdogGitHubRunner{}
	online := map[string]bool{}
	for _, repo := range repos {
		runners, err := opts.GitHubRunnersFn(ctx, repo)
		if err != nil {
			report.add(WatchdogEvent{Event: "runner-online-unknown", Severity: "warning", Repo: repo, Reason: "github-runners-failed", Detail: err.Error()})
			report.Exit = maxExit(report.Exit, 1)
			continue
		}
		byRepo[repo] = runners
		for _, r := range runners {
			if r.Status == "online" && hasWatchdogLabel(r.Labels, "civm") {
				online[repo] = true
				report.add(WatchdogEvent{Event: "runner-online", Severity: "info", Repo: repo, Runner: r.Name, Online: true})
			}
		}
	}
	return byRepo, online
}

func restartCandidates(systemd []Status, ghByRepo map[string][]WatchdogGitHubRunner) map[string]string {
	localByRepoName := map[string]Status{}
	out := map[string]string{}
	for _, s := range systemd {
		localByRepoName[s.Repo+"/"+s.Name] = s
		if s.UnitName == "" {
			continue
		}
		if s.ActiveState != "active" || s.SubState != "running" {
			out[s.UnitName] = "systemd-" + strings.Trim(strings.Join([]string{s.ActiveState, s.SubState}, "-"), "-")
		}
	}
	for repo, runners := range ghByRepo {
		for _, r := range runners {
			if r.Status != "offline" {
				continue
			}
			if local, ok := localByRepoName[repo+"/"+r.Name]; ok && local.UnitName != "" {
				out[local.UnitName] = "github-offline"
			}
		}
	}
	return out
}

func rerunnableConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "cancelled", "timed_out":
		return true
	default:
		return false
	}
}

type RerunMarker struct {
	Repo    string    `json:"repo"`
	RunID   int64     `json:"run_id"`
	HeadSHA string    `json:"head_sha"`
	RerunAt time.Time `json:"rerun_at"`
}

// autoRestartWindow is the per-unit, rolling-hour counter of sentinel-driven
// auto-restarts (anti restart-loop, DT-8). When NowFn - WindowStart >= 1h the
// Count window resets. LastActed is the timestamp of the newest sentinel this
// unit was already restarted for; it persists across windows so a single
// historical sentinel line (which stays in hooks.jsonl after we act) never
// re-triggers a restart of an already-recovered runner (dedup, F2).
type autoRestartWindow struct {
	Count       int       `json:"count"`
	WindowStart time.Time `json:"window_start"`
	LastActed   time.Time `json:"last_acted,omitempty"`
}

type queueStallIncident struct {
	Signature   string    `json:"signature"`
	FirstSeen   time.Time `json:"first_seen"`
	RestartedAt time.Time `json:"restarted_at,omitempty"`
}

type rerunState struct {
	Reruns       map[string]RerunMarker        `json:"reruns"`
	AutoRestarts map[string]autoRestartWindow  `json:"auto_restarts,omitempty"`
	QueueStalls  map[string]queueStallIncident `json:"queue_stalls,omitempty"`
}

func loadRerunState(opts WatchdogOptions) (rerunState, error) {
	state := rerunState{
		Reruns:       map[string]RerunMarker{},
		AutoRestarts: map[string]autoRestartWindow{},
		QueueStalls:  map[string]queueStallIncident{},
	}
	data, err := opts.ReadFileFn(opts.MarkerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return rerunState{Reruns: map[string]RerunMarker{}}, err
	}
	if state.Reruns == nil {
		state.Reruns = map[string]RerunMarker{}
	}
	if state.AutoRestarts == nil {
		state.AutoRestarts = map[string]autoRestartWindow{}
	}
	if state.QueueStalls == nil {
		state.QueueStalls = map[string]queueStallIncident{}
	}
	return state, nil
}

func writeRerunState(opts WatchdogOptions, state rerunState) error {
	dir := filepath.Dir(opts.MarkerPath)
	if err := opts.MkdirAllFn(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if opts.RenameFn != nil {
		tmpPath := opts.MarkerPath + ".tmp"
		if err := opts.WriteFileFn(tmpPath, data, 0644); err != nil {
			return err
		}
		if err := opts.RenameFn(tmpPath, opts.MarkerPath); err != nil {
			if opts.RemoveFn != nil {
				_ = opts.RemoveFn(tmpPath)
			}
			return err
		}
		return nil
	}
	return opts.WriteFileFn(opts.MarkerPath, data, 0644)
}

func rerunMarkerKey(runID int64, headSHA string) string {
	return fmt.Sprintf("%d/%s", runID, headSHA)
}

func applyWatchdogDefaults(opts *WatchdogOptions) {
	defaults := DefaultWatchdogOptions()
	applyWatchdogScalarDefaults(opts, defaults)
	applyWatchdogIOFnDefaults(opts, defaults)
	applyWatchdogCommandFnDefaults(opts)
}

// applyWatchdogScalarDefaults preenche cada campo escalar zero-valued com o
// default do mesmo campo. Tabela em vez de staircase: cada linha pareia o
// "está zerado?" do campo com a ação que aplica o default — o set de campos com
// fallback fica óbvio numa varredura só.
func applyWatchdogScalarDefaults(opts *WatchdogOptions, defaults WatchdogOptions) {
	scalarDefaults := []struct {
		isZero func() bool
		apply  func()
	}{
		{func() bool { return opts.NetworkTimeout == 0 }, func() { opts.NetworkTimeout = defaults.NetworkTimeout }},
		{func() bool { return opts.RestartDelay == 0 }, func() { opts.RestartDelay = defaults.RestartDelay }},
		{func() bool { return opts.MaxRunAge == 0 }, func() { opts.MaxRunAge = defaults.MaxRunAge }},
		{func() bool { return opts.RunLimit == 0 }, func() { opts.RunLimit = defaults.RunLimit }},
		{func() bool { return opts.MarkerPath == "" }, func() { opts.MarkerPath = defaults.MarkerPath }},
		{func() bool { return opts.MaintenanceStatePath == "" }, func() { opts.MaintenanceStatePath = defaults.MaintenanceStatePath }},
		{func() bool { return opts.HooksLogPath == "" }, func() { opts.HooksLogPath = defaults.HooksLogPath }},
		{func() bool { return opts.AutoRestartPerHour == 0 }, func() { opts.AutoRestartPerHour = defaults.AutoRestartPerHour }},
		{func() bool { return opts.QueueStallDwell == 0 }, func() { opts.QueueStallDwell = defaults.QueueStallDwell }},
	}
	for _, d := range scalarDefaults {
		if d.isZero() {
			d.apply()
		}
	}
}

// applyWatchdogIOFnDefaults preenche os hooks de I/O puro (sem closure sobre
// opts) com as implementações do DefaultWatchdogOptions. Idêntico ao bloco
// escalar, mas separado porque o tipo de campo é func, não valor.
func applyWatchdogIOFnDefaults(opts *WatchdogOptions, defaults WatchdogOptions) {
	if opts.RunFn == nil {
		opts.RunFn = defaults.RunFn
	}
	if opts.ActivityFn == nil {
		opts.ActivityFn = defaults.ActivityFn
	}
	if opts.HookInstallFn == nil {
		opts.HookInstallFn = defaults.HookInstallFn
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = defaults.ReadFileFn
	}
	if opts.WriteFileFn == nil {
		opts.WriteFileFn = defaults.WriteFileFn
	}
	if opts.MkdirAllFn == nil {
		opts.MkdirAllFn = defaults.MkdirAllFn
	}
	if opts.RenameFn == nil {
		opts.RenameFn = defaults.RenameFn
	}
	if opts.RemoveFn == nil {
		opts.RemoveFn = defaults.RemoveFn
	}
	if opts.MaintenanceActiveFn == nil {
		opts.MaintenanceActiveFn = defaults.MaintenanceActiveFn
	}
	if opts.NowFn == nil {
		opts.NowFn = defaults.NowFn
	}
	if opts.SleepFn == nil {
		opts.SleepFn = defaults.SleepFn
	}
}

// applyWatchdogCommandFnDefaults instala os hooks que precisam de closure sobre
// `opts` (cada um delega ao RunFn já resolvido, por isso rodam DEPOIS do I/O Fn
// default acima). Cada fallback é o caminho command-backed (gh / systemctl /
// git) usado fora dos testes herméticos.
func applyWatchdogCommandFnDefaults(opts *WatchdogOptions) {
	if opts.RestartFn == nil {
		opts.RestartFn = func(ctx context.Context, unit string) error {
			return defaultWatchdogRestart(ctx, *opts, unit)
		}
	}
	if opts.SystemRunnersFn == nil {
		opts.SystemRunnersFn = func(ctx context.Context) ([]Status, error) {
			listOpts := DefaultListOptions()
			listOpts.RunFn = opts.RunFn
			return List(ctx, listOpts)
		}
	}
	if opts.GitHubRunnersFn == nil {
		opts.GitHubRunnersFn = func(ctx context.Context, repo string) ([]WatchdogGitHubRunner, error) {
			return listWatchdogGitHubRunners(ctx, repo, opts.RunFn)
		}
	}
	if opts.ListRunsFn == nil {
		opts.ListRunsFn = func(ctx context.Context, repo string, limit int) ([]WatchdogRun, error) {
			return listWatchdogRuns(ctx, repo, limit, opts.RunFn)
		}
	}
	if opts.PullRequestFn == nil {
		opts.PullRequestFn = func(ctx context.Context, repo string, number int) (WatchdogPullRequest, error) {
			return getWatchdogPullRequest(ctx, repo, number, opts.RunFn)
		}
	}
	if opts.RunLogFn == nil {
		opts.RunLogFn = func(ctx context.Context, repo string, runID int64) (string, error) {
			return getWatchdogRunLog(ctx, repo, runID, opts.RunFn)
		}
	}
	if opts.RerunFn == nil {
		opts.RerunFn = func(ctx context.Context, repo string, runID int64) error {
			_, err := opts.RunFn(ctx, "gh", "run", "rerun", strconv.FormatInt(runID, 10), "--repo", repo, "--failed")
			return err
		}
	}
	if opts.QueueReposFn == nil {
		opts.QueueReposFn = defaultWatchdogQueueRepos
	}
	if opts.QueuedCivmJobsFn == nil {
		opts.QueuedCivmJobsFn = func(
			ctx context.Context,
			repos []string,
			now time.Time,
			maxAge time.Duration,
		) (WatchdogQueueEvidence, error) {
			return listQueuedCivmJobs(ctx, repos, now, maxAge, opts.RunFn)
		}
	}
	if opts.NetworkFn == nil {
		opts.NetworkFn = func(ctx context.Context, timeout time.Duration) error {
			networkCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			_, err := opts.RunFn(networkCtx, "git", "ls-remote", "https://github.com/actions/checkout.git", "HEAD")
			if err != nil {
				return fmt.Errorf("git ls-remote github.com: %w", err)
			}
			return nil
		}
	}
}

func validateWatchdogOptions(opts WatchdogOptions) error {
	for _, repo := range opts.Repos {
		if err := civm.ValidateRepo(repo); err != nil {
			return err
		}
	}
	if opts.NetworkTimeout <= 0 {
		return fmt.Errorf("--network-timeout deve ser >0")
	}
	if opts.RestartDelay < 0 {
		return fmt.Errorf("--restart-delay deve ser >=0")
	}
	if opts.MaxRunAge <= 0 {
		return fmt.Errorf("--max-run-age deve ser >0")
	}
	if opts.RunLimit < 1 || opts.RunLimit > 100 {
		return fmt.Errorf("run limit deve ficar entre 1 e 100")
	}
	if opts.QueueStallDwell <= 0 {
		return fmt.Errorf("queue stall dwell deve ser >0")
	}
	if strings.ContainsRune(opts.MarkerPath, 0) || !filepath.IsAbs(filepath.Clean(opts.MarkerPath)) {
		return fmt.Errorf("marker path deve ser absoluto e seguro")
	}
	return nil
}

func listWatchdogGitHubRunners(ctx context.Context, repo string, runFn func(context.Context, string, ...string) ([]byte, error)) ([]WatchdogGitHubRunner, error) {
	out, err := runFn(ctx, "gh", "api", fmt.Sprintf("/repos/%s/actions/runners", repo))
	if err != nil {
		return nil, fmt.Errorf("gh api actions/runners: %w", err)
	}
	return parseWatchdogGitHubRunners(repo, out)
}

func parseWatchdogGitHubRunners(scope string, data []byte) ([]WatchdogGitHubRunner, error) {
	var raw struct {
		Runners []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Busy   bool   `json:"busy"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse gh runners: %w", err)
	}
	runners := make([]WatchdogGitHubRunner, 0, len(raw.Runners))
	for _, rr := range raw.Runners {
		item := WatchdogGitHubRunner{ID: rr.ID, Repo: scope, Name: rr.Name, Status: rr.Status, Busy: rr.Busy}
		for _, label := range rr.Labels {
			item.Labels = append(item.Labels, label.Name)
		}
		runners = append(runners, item)
	}
	return runners, nil
}

func defaultWatchdogQueueRepos() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv("CIVM_REAPER_REPOS"))
	if raw == "" {
		return nil, nil
	}
	var repos []string
	seen := map[string]struct{}{}
	for _, repo := range strings.Split(raw, ",") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if err := civm.ValidateRepo(repo); err != nil {
			return nil, fmt.Errorf("CIVM_REAPER_REPOS: %w", err)
		}
		if _, duplicate := seen[repo]; duplicate {
			continue
		}
		seen[repo] = struct{}{}
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos, nil
}

func listQueuedCivmJobs(
	ctx context.Context,
	repos []string,
	now time.Time,
	maxAge time.Duration,
	runFn func(context.Context, string, ...string) ([]byte, error),
) (WatchdogQueueEvidence, error) {
	var candidates []watchdogQueuedCandidate
	seen := make(map[string]struct{})
	for _, repo := range repos {
		if err := civm.ValidateRepo(repo); err != nil {
			return WatchdogQueueEvidence{}, err
		}
		runs, err := listFreshQueuedRuns(ctx, repo, now, maxAge, runFn)
		if err != nil {
			return WatchdogQueueEvidence{}, err
		}
		for _, run := range runs {
			jobIDs, err := listEligibleCivmJobIDs(ctx, repo, run.ID, runFn)
			if err != nil {
				return WatchdogQueueEvidence{}, err
			}
			for _, jobID := range jobIDs {
				identity := fmt.Sprintf("%s/%020d/%020d", repo, run.ID, jobID)
				if _, duplicate := seen[identity]; duplicate {
					continue
				}
				seen[identity] = struct{}{}
				candidates = append(candidates, watchdogQueuedCandidate{
					CreatedAt: run.CreatedAt,
					Identity:  identity,
				})
			}
			if len(jobIDs) > 0 {
				break
			}
		}
	}
	if len(candidates) == 0 {
		return WatchdogQueueEvidence{}, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].Identity < candidates[j].Identity
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	oldest := sha256.Sum256([]byte(candidates[0].Identity))
	return WatchdogQueueEvidence{
		EligibleJobs: len(candidates),
		Signature:    fmt.Sprintf("%x", oldest),
	}, nil
}

func listFreshQueuedRuns(
	ctx context.Context,
	repo string,
	now time.Time,
	maxAge time.Duration,
	runFn func(context.Context, string, ...string) ([]byte, error),
) ([]watchdogQueuedRun, error) {
	endpoint := fmt.Sprintf("/repos/%s/actions/runs?per_page=100&status=queued", repo)
	out, err := runFn(ctx, "gh", "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("list queued runs for %s: %w", repo, err)
	}
	var pages []struct {
		WorkflowRuns []struct {
			ID        int64     `json:"id"`
			Status    string    `json:"status"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("parse queued runs for %s: %w", repo, err)
	}
	var runs []watchdogQueuedRun
	seen := make(map[int64]struct{})
	for _, page := range pages {
		for _, run := range page.WorkflowRuns {
			age := now.Sub(run.CreatedAt)
			if run.Status != "queued" || run.ID <= 0 || run.CreatedAt.IsZero() ||
				age < 0 || age > maxAge {
				continue
			}
			if _, duplicate := seen[run.ID]; duplicate {
				continue
			}
			seen[run.ID] = struct{}{}
			runs = append(runs, watchdogQueuedRun{ID: run.ID, CreatedAt: run.CreatedAt})
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].CreatedAt.Before(runs[j].CreatedAt)
	})
	return runs, nil
}

func listEligibleCivmJobIDs(
	ctx context.Context,
	repo string,
	runID int64,
	runFn func(context.Context, string, ...string) ([]byte, error),
) ([]int64, error) {
	endpoint := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, runID)
	out, err := runFn(ctx, "gh", "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("list queued jobs for %s: %w", repo, err)
	}
	var pages []struct {
		Jobs []struct {
			ID     int64    `json:"id"`
			Status string   `json:"status"`
			Labels []string `json:"labels"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("parse queued jobs for %s: %w", repo, err)
	}
	var ids []int64
	seen := make(map[int64]struct{})
	for _, page := range pages {
		for _, job := range page.Jobs {
			if job.Status != "queued" || job.ID <= 0 ||
				!hasWatchdogLabel(job.Labels, "self-hosted") ||
				!hasWatchdogLabel(job.Labels, "civm") {
				continue
			}
			if _, duplicate := seen[job.ID]; duplicate {
				continue
			}
			seen[job.ID] = struct{}{}
			ids = append(ids, job.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func listWatchdogRuns(ctx context.Context, repo string, limit int, runFn func(context.Context, string, ...string) ([]byte, error)) ([]WatchdogRun, error) {
	endpoint := fmt.Sprintf("/repos/%s/actions/runs?per_page=%d&status=completed", repo, limit)
	out, err := runFn(ctx, "gh", "api", endpoint)
	if err != nil {
		return nil, fmt.Errorf("gh api actions/runs: %w", err)
	}
	var raw struct {
		WorkflowRuns []struct {
			ID           int64     `json:"id"`
			HeadSHA      string    `json:"head_sha"`
			Status       string    `json:"status"`
			Conclusion   string    `json:"conclusion"`
			CreatedAt    time.Time `json:"created_at"`
			URL          string    `json:"html_url"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh runs: %w", err)
	}
	runs := make([]WatchdogRun, 0, len(raw.WorkflowRuns))
	for _, rr := range raw.WorkflowRuns {
		run := WatchdogRun{
			ID:         rr.ID,
			HeadSHA:    rr.HeadSHA,
			Status:     rr.Status,
			Conclusion: rr.Conclusion,
			CreatedAt:  rr.CreatedAt,
			URL:        rr.URL,
		}
		for _, pr := range rr.PullRequests {
			run.PullRequests = append(run.PullRequests, WatchdogPullRequestRef{Number: pr.Number})
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func getWatchdogPullRequest(ctx context.Context, repo string, number int, runFn func(context.Context, string, ...string) ([]byte, error)) (WatchdogPullRequest, error) {
	out, err := runFn(ctx, "gh", "api", fmt.Sprintf("/repos/%s/pulls/%d", repo, number))
	if err != nil {
		return WatchdogPullRequest{}, fmt.Errorf("gh api pulls/%d: %w", number, err)
	}
	var raw struct {
		Number         int    `json:"number"`
		State          string `json:"state"`
		MergeableState string `json:"mergeable_state"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return WatchdogPullRequest{}, fmt.Errorf("parse gh pull: %w", err)
	}
	return WatchdogPullRequest{Number: raw.Number, State: raw.State, MergeableState: raw.MergeableState}, nil
}

func getWatchdogRunLog(ctx context.Context, repo string, runID int64, runFn func(context.Context, string, ...string) ([]byte, error)) (string, error) {
	out, err := runFn(ctx, "gh", "run", "view", strconv.FormatInt(runID, 10), "--repo", repo, "--log-failed")
	if err != nil {
		return "", fmt.Errorf("gh run view --log-failed: %w", err)
	}
	return string(out), nil
}

func inferWatchdogRepos(systemd []Status) []string {
	seen := map[string]bool{}
	var repos []string
	for _, status := range systemd {
		if status.Repo == "" || seen[status.Repo] || civm.ValidateRepo(status.Repo) != nil {
			continue
		}
		seen[status.Repo] = true
		repos = append(repos, status.Repo)
	}
	sort.Strings(repos)
	return repos
}

func enrichWatchdogSystemdRepos(ctx context.Context, opts WatchdogOptions, systemd []Status) []Status {
	out := append([]Status(nil), systemd...)
	for i := range out {
		repo, dir, ok := resolveWatchdogRunnerRepo(ctx, opts, out[i])
		if dir != "" {
			out[i].WorkingDirectory = dir
		}
		if ok {
			out[i].Repo = repo
		}
		if org, _, ok := resolveWatchdogRunnerOrg(ctx, opts, out[i]); ok {
			out[i].Org = org
		}
	}
	return out
}

func resolveWatchdogRunnerRepo(ctx context.Context, opts WatchdogOptions, status Status) (repo string, dir string, ok bool) {
	if status.UnitName == "" {
		return "", "", false
	}
	if err := civm.ValidateServiceUnit(status.UnitName); err != nil {
		return "", "", false
	}
	out, err := opts.RunFn(ctx, "systemctl", "show", status.UnitName, "--property=WorkingDirectory", "--value")
	if err != nil {
		return "", "", false
	}
	dir = strings.TrimSpace(string(out))
	if dir == "" || dir == "-" {
		return "", "", false
	}
	data, err := opts.ReadFileFn(filepath.Join(dir, ".runner"))
	if err != nil {
		// WorkingDirectory is known (usable for sentinel→unit mapping) even when
		// the .runner repo cannot be read.
		return "", dir, false
	}
	repo, ok = repoFromRunnerConfig(data)
	return repo, dir, ok
}

func resolveWatchdogRunnerOrg(ctx context.Context, opts WatchdogOptions, status Status) (org string, dir string, ok bool) {
	if status.UnitName == "" {
		return "", "", false
	}
	if err := civm.ValidateServiceUnit(status.UnitName); err != nil {
		return "", "", false
	}
	out, err := opts.RunFn(ctx, "systemctl", "show", status.UnitName, "--property=WorkingDirectory", "--value")
	if err != nil {
		return "", "", false
	}
	dir = strings.TrimSpace(string(out))
	if dir == "" || dir == "-" {
		return "", "", false
	}
	data, err := opts.ReadFileFn(filepath.Join(dir, ".runner"))
	if err != nil {
		return "", dir, false
	}
	org, ok = orgFromRunnerConfig(data)
	return org, dir, ok
}

func repoFromRunnerConfig(data []byte) (string, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimJSONBOM(data), &raw); err != nil {
		return "", false
	}
	for _, field := range []string{"gitHubUrl", "serverUrl"} {
		valueRaw, ok := raw[field]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(valueRaw, &value); err != nil {
			continue
		}
		if repo, ok := repoFromGitHubURL(value); ok {
			return repo, true
		}
	}
	return "", false
}

func orgFromRunnerConfig(data []byte) (string, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimJSONBOM(data), &raw); err != nil {
		return "", false
	}
	for _, field := range []string{"gitHubUrl", "serverUrl"} {
		valueRaw, ok := raw[field]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(valueRaw, &value); err != nil {
			continue
		}
		if org, ok := orgFromGitHubURL(value); ok {
			return org, true
		}
	}
	return "", false
}

func trimJSONBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
}

func orgFromGitHubURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 1 || parts[0] == "" || strings.Contains(parts[0], ".git") {
		return "", false
	}
	return parts[0], true
}

func listWatchdogGitHubOrgRunners(ctx context.Context, org string, runFn func(context.Context, string, ...string) ([]byte, error)) ([]WatchdogGitHubRunner, error) {
	out, err := runFn(ctx, "gh", "api", fmt.Sprintf("/orgs/%s/actions/runners", org))
	if err != nil {
		return nil, fmt.Errorf("gh api org actions/runners: %w", err)
	}
	return parseWatchdogGitHubRunners(org, out)
}

func repoFromGitHubURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return "", false
	}
	repoName := strings.TrimSuffix(parts[1], ".git")
	repo := parts[0] + "/" + repoName
	if civm.ValidateRepo(repo) != nil {
		return "", false
	}
	return repo, true
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func anyRepoOnline(repoOnline map[string]bool) bool {
	for _, online := range repoOnline {
		if online {
			return true
		}
	}
	return false
}

func anyLocalRunnerOnline(systemd []Status) bool {
	for _, s := range systemd {
		if s.ActiveState == "active" && s.SubState == "running" {
			return true
		}
	}
	return false
}

func watchdogReportHasEvent(report WatchdogReport, event string) bool {
	for _, got := range report.Events {
		if got.Event == event {
			return true
		}
	}
	return false
}

func hasWatchdogLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func maxExit(current, candidate int) int {
	if candidate > current {
		return candidate
	}
	return current
}

func (r *WatchdogReport) add(event WatchdogEvent) {
	if event.Severity == "" {
		event.Severity = "info"
	}
	switch event.Event {
	case "rerun-skipped":
		r.Metrics.RerunsSkipped++
	case "rerun-triggered":
		r.Metrics.RerunsTriggered++
	}
	r.Events = append(r.Events, event)
}

func (r WatchdogReport) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func (r WatchdogReport) Render(w io.Writer) {
	mode := "DRY-RUN"
	if r.Executed {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "civmctl runner watchdog: %s | exit=%d | runner_online=%v\n", mode, r.Exit, r.RunnerOnline)
	fmt.Fprintf(w, "Rerun metrics: runs_considered=%d reruns_triggered=%d reruns_skipped=%d\n",
		r.Metrics.RunsConsidered, r.Metrics.RerunsTriggered, r.Metrics.RerunsSkipped)
	fmt.Fprintf(
		w,
		"Queue metrics: stalls=%d runner_restarts=%d unresolved=%d\n",
		r.Metrics.QueueStalls,
		r.Metrics.RunnerRestarts,
		r.Metrics.StallsUnresolved,
	)
	if len(r.Repos) > 0 {
		fmt.Fprintf(w, "Repos: %s\n", strings.Join(r.Repos, ","))
	}
	for _, event := range r.Events {
		target := event.Repo
		if event.Unit != "" {
			target = event.Unit
		}
		if target == "" && event.Runner != "" {
			target = event.Runner
		}
		if target == "" {
			target = "-"
		}
		detail := event.Detail
		if event.Reason != "" {
			detail = event.Reason
			if event.Detail != "" {
				detail += ": " + event.Detail
			}
		}
		if event.RunID != 0 {
			target = fmt.Sprintf("%s run=%d", target, event.RunID)
		}
		if event.QueuedJobs != 0 {
			detail = fmt.Sprintf("%s queued=%d age_seconds=%d", detail, event.QueuedJobs, event.AgeSeconds)
		}
		fmt.Fprintf(w, "  %-18s %-8s %-64s %s\n", event.Event, event.Severity, target, detail)
	}
}
