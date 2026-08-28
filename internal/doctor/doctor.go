// Package doctor consolidates read-only civm host and GitHub runner state.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/health"
	"github.com/emersonbusson/civm/internal/hook"
	"github.com/emersonbusson/civm/internal/runner"
)

// DefaultRepos is used only when callers request the explicit "default" repo
// mode. It is intentionally empty for a public/generic civm — operators pass
// --repos=auto (infer local runners) or an explicit owner/repo list. Product-
// specific fleets must not ship as code defaults.
var DefaultRepos = []string{}

type Severity string

const (
	SeverityOK       Severity = "ok"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

func (s Severity) ExitCode() int {
	switch s {
	case SeverityOK:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

type HostCheck struct {
	Name     string `json:"name"`
	Detail   string `json:"detail"`
	Severity string `json:"severity"`
}

type HookCheck struct {
	Name     string   `json:"name"`
	Detail   string   `json:"detail"`
	Severity Severity `json:"severity"`
}

type SystemdRunner struct {
	UnitName    string `json:"unit_name"`
	Repo        string `json:"repo"`
	Name        string `json:"name"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
}

type GitHubRunner struct {
	ID     int64    `json:"id,omitempty"`
	Repo   string   `json:"repo"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Busy   bool     `json:"busy"`
	Labels []string `json:"labels"`
}

type RunnerDiagnosis struct {
	ID                  int64    `json:"id,omitempty"`
	Repo                string   `json:"repo"`
	Name                string   `json:"name"`
	Status              string   `json:"status"`
	Busy                bool     `json:"busy"`
	Labels              []string `json:"labels,omitempty"`
	Classification      string   `json:"classification"`
	Severity            Severity `json:"severity"`
	Detail              string   `json:"detail"`
	ManualRemoveCommand string   `json:"manual_remove_command,omitempty"`
}

type RepoDiagnosis struct {
	Repo     string            `json:"repo"`
	Severity Severity          `json:"severity"`
	Runners  []RunnerDiagnosis `json:"runners"`
}

type Report struct {
	WorkflowFile   string          `json:"workflow_file"`
	HostChecks     []HostCheck     `json:"host_checks"`
	HookChecks     []HookCheck     `json:"hook_checks"`
	SystemdRunners []SystemdRunner `json:"systemd_runners"`
	GitHubRepos    []RepoDiagnosis `json:"github_repos"`
	Exit           int             `json:"exit"`
}

type Options struct {
	Repos          []string
	InferRepos     bool
	WorkflowFile   string
	WorkDir        string
	HooksDir       string
	CivmctlPath    string
	RunnerGlob     string
	UnitsSourceDir string

	HealthFn        func(ctx context.Context) health.Report
	SystemRunnersFn func(ctx context.Context) ([]runner.Status, error)
	GitHubRunnersFn func(ctx context.Context, repo string) ([]GitHubRunner, error)
	RunFn           func(ctx context.Context, name string, args ...string) ([]byte, error)
	GlobFn          func(pattern string) ([]string, error)
	ReadFileFn      func(path string) ([]byte, error)
	StatFn          func(path string) (os.FileInfo, error)
}

func DefaultOptions() Options {
	return Options{
		InferRepos:     true,
		WorkflowFile:   "ci.yml",
		WorkDir:        civm.DefaultHealthDiskPath,
		HooksDir:       hook.DefaultHooksDir,
		CivmctlPath:    hook.DefaultCivmctlBin,
		RunnerGlob:     hook.DefaultRunnerGlob,
		UnitsSourceDir: civm.DefaultUnitsSourceDir,
		RunFn:          defaultRun,
	}
}

func Collect(ctx context.Context, opts Options) (Report, error) {
	applyDefaults(&opts)
	if err := validateOptions(opts); err != nil {
		return Report{}, err
	}
	report := Report{
		WorkflowFile:   opts.WorkflowFile,
		HostChecks:     []HostCheck{},
		HookChecks:     []HookCheck{},
		SystemdRunners: []SystemdRunner{},
		GitHubRepos:    []RepoDiagnosis{},
	}

	host := opts.HealthFn(ctx)
	worst := SeverityOK
	for _, c := range host.Checks {
		sev := severityFromHealth(c.Status)
		report.HostChecks = append(report.HostChecks, HostCheck{
			Name:     c.Name,
			Detail:   c.Detail,
			Severity: string(sev),
		})
		worst = maxSeverity(worst, sev)
	}

	// SPECv3 ITEM-7: cgroup v2 + run-as-runner-user (DT-v3-1) + RAM-fit invariant.
	admitChecks, admitSeverity := collectAdmitChecks(ctx, opts)
	report.HostChecks = append(report.HostChecks, admitChecks...)
	worst = maxSeverity(worst, admitSeverity)

	systemd, systemdErr := opts.SystemRunnersFn(ctx)
	if systemdErr != nil {
		worst = maxSeverity(worst, SeverityWarning)
		report.SystemdRunners = append(report.SystemdRunners, SystemdRunner{
			UnitName:    "(unknown)",
			ActiveState: "unknown",
			SubState:    systemdErr.Error(),
		})
	} else {
		for _, r := range systemd {
			report.SystemdRunners = append(report.SystemdRunners, SystemdRunner{
				UnitName: r.UnitName, Repo: r.Repo, Name: r.Name,
				ActiveState: r.ActiveState, SubState: r.SubState,
			})
		}
	}

	hookChecks, hookSeverity := collectHookChecks(ctx, opts, systemd, systemdErr)
	report.HookChecks = hookChecks
	worst = maxSeverity(worst, hookSeverity)

	repos := append([]string(nil), opts.Repos...)
	if opts.InferRepos && len(repos) == 0 && systemdErr == nil {
		repos = inferReposFromSystemd(systemd)
	}
	if err := validateRepos(repos); err != nil {
		return Report{}, err
	}
	for _, repo := range repos {
		runners, err := opts.GitHubRunnersFn(ctx, repo)
		diag := ClassifyRepo(repo, runners, err)
		report.GitHubRepos = append(report.GitHubRepos, diag)
		worst = maxSeverity(worst, diag.Severity)
	}

	report.Exit = worst.ExitCode()
	return report, nil
}

func applyDefaults(opts *Options) {
	if opts.WorkflowFile == "" {
		opts.WorkflowFile = "ci.yml"
	}
	if opts.WorkDir == "" {
		opts.WorkDir = civm.DefaultHealthDiskPath
	}
	if opts.HooksDir == "" {
		opts.HooksDir = hook.DefaultHooksDir
	}
	if opts.CivmctlPath == "" {
		opts.CivmctlPath = hook.DefaultCivmctlBin
	}
	if opts.RunnerGlob == "" {
		opts.RunnerGlob = hook.DefaultRunnerGlob
	}
	if opts.UnitsSourceDir == "" {
		opts.UnitsSourceDir = civm.DefaultUnitsSourceDir
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.GlobFn == nil {
		opts.GlobFn = filepath.Glob
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = os.ReadFile
	}
	if opts.StatFn == nil {
		opts.StatFn = os.Stat
	}
	if opts.HealthFn == nil {
		opts.HealthFn = func(ctx context.Context) health.Report {
			return health.NewDefaultCollector(opts.WorkDir).Collect(ctx)
		}
	}
	if opts.SystemRunnersFn == nil {
		opts.SystemRunnersFn = func(ctx context.Context) ([]runner.Status, error) {
			listOpts := runner.DefaultListOptions()
			listOpts.RunFn = opts.RunFn
			return runner.List(ctx, listOpts)
		}
	}
	if opts.GitHubRunnersFn == nil {
		opts.GitHubRunnersFn = func(ctx context.Context, repo string) ([]GitHubRunner, error) {
			return listGitHubRunners(ctx, repo, opts.RunFn)
		}
	}
}

func validateOptions(opts Options) error {
	if err := validateRepos(opts.Repos); err != nil {
		return err
	}
	return civm.ValidateWorkflowFile(opts.WorkflowFile)
}

func validateRepos(repos []string) error {
	for _, repo := range repos {
		if err := civm.ValidateRepo(repo); err != nil {
			return err
		}
	}
	return nil
}

func inferReposFromSystemd(systemd []runner.Status) []string {
	seen := map[string]bool{}
	var repos []string
	for _, status := range systemd {
		if status.Repo == "" || seen[status.Repo] {
			continue
		}
		if civm.ValidateRepo(status.Repo) != nil {
			continue
		}
		seen[status.Repo] = true
		repos = append(repos, status.Repo)
	}
	sort.Strings(repos)
	return repos
}

func collectHookChecks(ctx context.Context, opts Options, systemd []runner.Status, systemdErr error) ([]HookCheck, Severity) {
	checks := []HookCheck{
		checkHookScript(opts, "HOOK_JOB_STARTED", hook.StartedHookName, hook.EventJobStarted),
		checkHookScript(opts, "HOOK_JOB_COMPLETED", hook.CompletedHookName, hook.EventJobCompleted),
		checkRunnerEnvHooks(opts),
		checkRunnerServices(systemd, systemdErr),
		checkRunnerSerialization(systemd, systemdErr),
		checkScopedSudoers(ctx, opts),
		checkGenerationBoundaryCapability(ctx, opts),
		checkUnitScriptsInstalled(opts),
	}
	worst := SeverityOK
	for _, check := range checks {
		worst = maxSeverity(worst, check.Severity)
	}
	return checks, worst
}

// checkScopedSudoers proves the privileged-delete escalation capability the way
// safedelete will actually use it at runtime, instead of reading the sudoers
// drop-in — which is 0440 root:root (unreadable as the runner user) and whose
// existence is not function. It runs the wrapper's no-op self-check under
// sudo -n: a secure_path mismatch, a missing NOPASSWD rule or a removed wrapper
// all surface here as the capability being gone (DT-v2-10; testing.md
// "existence != function"). Mirrors install's verifySafeDeleteCapability so the
// ongoing health check catches drift after a clean install.
func checkScopedSudoers(ctx context.Context, opts Options) HookCheck {
	if _, err := opts.RunFn(ctx, "sudo", "-n", civm.DefaultSafeDeleteWrapperPath, "--check"); err != nil {
		return HookCheck{
			Name:     "SCOPED_SUDOERS",
			Severity: SeverityCritical,
			Detail:   fmt.Sprintf("sudo -n %s --check failed (%v); run sudo civmctl hook install --execute", civm.DefaultSafeDeleteWrapperPath, err),
		}
	}
	return HookCheck{
		Name:     "SCOPED_SUDOERS",
		Severity: SeverityOK,
		Detail:   fmt.Sprintf("sudo -n %s --check ok", civm.DefaultSafeDeleteWrapperPath),
	}
}

// checkGenerationBoundaryCapability proves the exact versioned protocol used
// by civm-host before it can request a guest drain, cleanup or poweroff. A
// successful sudo syntax check is insufficient: the wrapper must be invokable
// and its civmctl capability marker must match the host contract.
func checkGenerationBoundaryCapability(ctx context.Context, opts Options) HookCheck {
	out, err := opts.RunFn(ctx, "sudo", "-n", civm.DefaultGenerationBoundaryWrapperPath, "--check")
	if err != nil {
		return HookCheck{
			Name:     "GENERATION_BOUNDARY_CAPABILITY",
			Severity: SeverityCritical,
			Detail: fmt.Sprintf("sudo -n %s --check failed (%v); deploy civmctl and run sudo civmctl hook install --execute --no-restart",
				civm.DefaultGenerationBoundaryWrapperPath, err),
		}
	}
	if strings.TrimSpace(string(out)) != civm.GenerationCleanBoundaryMarker {
		return HookCheck{
			Name:     "GENERATION_BOUNDARY_CAPABILITY",
			Severity: SeverityCritical,
			Detail: fmt.Sprintf("sudo -n %s --check returned incompatible marker %q; deploy matching civmctl and wrapper",
				civm.DefaultGenerationBoundaryWrapperPath, strings.TrimSpace(string(out))),
		}
	}
	return HookCheck{
		Name:     "GENERATION_BOUNDARY_CAPABILITY",
		Severity: SeverityOK,
		Detail:   fmt.Sprintf("sudo -n %s --check returned %s", civm.DefaultGenerationBoundaryWrapperPath, civm.GenerationCleanBoundaryMarker),
	}
}

func checkHookScript(opts Options, checkName, hookName string, event hook.Event) HookCheck {
	path := filepath.Join(opts.HooksDir, hookName)
	data, err := opts.ReadFileFn(path)
	if err != nil {
		detail := fmt.Sprintf("read %s: %v", path, err)
		if os.IsNotExist(err) {
			detail = fmt.Sprintf("%s missing; run sudo civmctl hook install --execute", path)
		}
		return HookCheck{Name: checkName, Severity: SeverityCritical, Detail: detail}
	}
	want := hook.ScriptContent(opts.CivmctlPath, event)
	if string(data) != want {
		return HookCheck{
			Name:     checkName,
			Severity: SeverityCritical,
			Detail:   fmt.Sprintf("%s content mismatch; run sudo civmctl hook install --execute", path),
		}
	}
	return HookCheck{
		Name:     checkName,
		Severity: SeverityOK,
		Detail:   fmt.Sprintf("%s execs %s hook %s", path, opts.CivmctlPath, event),
	}
}

func checkRunnerEnvHooks(opts Options) HookCheck {
	runners, err := opts.GlobFn(opts.RunnerGlob)
	if err != nil {
		return HookCheck{Name: "HOOK_RUNNER_ENVS", Severity: SeverityWarning, Detail: fmt.Sprintf("glob %s: %v", opts.RunnerGlob, err)}
	}
	sort.Strings(runners)
	wantStarted := "ACTIONS_RUNNER_HOOK_JOB_STARTED=" + filepath.Join(opts.HooksDir, hook.StartedHookName)
	wantCompleted := "ACTIONS_RUNNER_HOOK_JOB_COMPLETED=" + filepath.Join(opts.HooksDir, hook.CompletedHookName)
	checked := 0
	var bad []string
	for _, runnerDir := range runners {
		if !hook.IsRunnerDirCandidate(runnerDir) {
			continue
		}
		checked++
		envPath := filepath.Join(runnerDir, ".env")
		data, err := opts.ReadFileFn(envPath)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s read failed: %v", envPath, err))
			continue
		}
		started, completed := parseHookEnvLines(string(data))
		if started != wantStarted || completed != wantCompleted {
			bad = append(bad, fmt.Sprintf("%s has %q / %q, want %q / %q", envPath, started, completed, wantStarted, wantCompleted))
		}
	}
	if checked == 0 {
		return HookCheck{Name: "HOOK_RUNNER_ENVS", Severity: SeverityWarning, Detail: fmt.Sprintf("nenhum runner .env encontrado por %s", opts.RunnerGlob)}
	}
	if len(bad) > 0 {
		return HookCheck{Name: "HOOK_RUNNER_ENVS", Severity: SeverityCritical, Detail: strings.Join(bad, "; ")}
	}
	return HookCheck{Name: "HOOK_RUNNER_ENVS", Severity: SeverityOK, Detail: fmt.Sprintf("%d runner .env files use civmctl hook scripts", checked)}
}

func parseHookEnvLines(content string) (started, completed string) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ACTIONS_RUNNER_HOOK_JOB_STARTED=") {
			started = line
		}
		if strings.HasPrefix(line, "ACTIONS_RUNNER_HOOK_JOB_COMPLETED=") {
			completed = line
		}
	}
	return started, completed
}

func checkRunnerServices(systemd []runner.Status, systemdErr error) HookCheck {
	if systemdErr != nil {
		return HookCheck{Name: "RUNNER_SERVICES", Severity: SeverityWarning, Detail: fmt.Sprintf("systemd runner status unknown: %v", systemdErr)}
	}
	if len(systemd) == 0 {
		return HookCheck{Name: "RUNNER_SERVICES", Severity: SeverityCritical, Detail: "0 actions.runner.* services found"}
	}
	active := 0
	var inactive []string
	for _, status := range systemd {
		if status.ActiveState == "active" && status.SubState == "running" {
			active++
			continue
		}
		inactive = append(inactive, fmt.Sprintf("%s(%s/%s)", status.UnitName, status.ActiveState, status.SubState))
	}
	detail := fmt.Sprintf("%d/%d actions.runner.* services active/running", active, len(systemd))
	if len(inactive) > 0 {
		return HookCheck{Name: "RUNNER_SERVICES", Severity: SeverityCritical, Detail: detail + "; inactive: " + strings.Join(inactive, ", ")}
	}
	return HookCheck{Name: "RUNNER_SERVICES", Severity: SeverityOK, Detail: detail}
}

// checkRunnerSerialization impõe o invariante de serialização do box: para cada
// organização com runner org presente, NENHUM runner por-repo da mesma org pode
// coexistir. Dois runners elegíveis para o mesmo repo (org + por-repo, ambos
// label civm) = dois jobs concorrentes no mesmo disco → "concurrent prune on
// shared civm runner" mata o docker pull (incidente #1184, validation.md
// 2026-06-18). Crítico quando há colisão; aponta o comando de remoção durável
// (NÃO `systemctl disable`, que o runner-watchdog ressuscita).
func checkRunnerSerialization(systemd []runner.Status, systemdErr error) HookCheck {
	if systemdErr != nil {
		return HookCheck{Name: "RUNNER_SERIALIZATION", Severity: SeverityWarning, Detail: fmt.Sprintf("systemd runner status unknown: %v", systemdErr)}
	}
	collisions := runner.DetectCollisions(systemd)
	if len(collisions) == 0 {
		return HookCheck{Name: "RUNNER_SERIALIZATION", Severity: SeverityOK, Detail: "1 runner por org (sem runner por-repo redundante)"}
	}
	parts := make([]string, 0, len(collisions))
	for _, c := range collisions {
		parts = append(parts, fmt.Sprintf("%s (%s) duplica %s para %s — remover: civmctl runner remove --short=%s --token=$(gh api -X POST /repos/%s/actions/runners/remove-token --jq .token) --execute",
			c.RepoUnit, c.RepoName, c.OrgName, c.Repo, repoRunnerShort(c.RepoName), c.Repo))
	}
	return HookCheck{
		Name:     "RUNNER_SERIALIZATION",
		Severity: SeverityCritical,
		Detail:   "runner por-repo redundante (quebra serialização): " + strings.Join(parts, "; "),
	}
}

// repoRunnerShort deriva o sufixo --short do nome do runner ("civm-app" ->
// "acme") para montar o comando de remoção. Sem o prefixo civm-, devolve o
// nome inteiro (o operador ajusta se necessário).
func repoRunnerShort(name string) string {
	return strings.TrimPrefix(name, "civm-")
}

func ClassifyRepo(repo string, runners []GitHubRunner, err error) RepoDiagnosis {
	out := RepoDiagnosis{Repo: repo, Severity: SeverityOK, Runners: []RunnerDiagnosis{}}
	if err != nil {
		out.Severity = SeverityWarning
		out.Runners = append(out.Runners, RunnerDiagnosis{
			Repo: repo, Name: "(unknown)", Status: "unknown",
			Classification: "unknown", Severity: SeverityWarning,
			Detail: fmt.Sprintf("nao foi possivel consultar GitHub runners: %v", err),
		})
		return out
	}
	if len(runners) == 0 {
		out.Severity = SeverityWarning
		out.Runners = append(out.Runners, RunnerDiagnosis{
			Repo: repo, Name: "(none)", Status: "missing",
			Classification: "missing", Severity: SeverityWarning,
			Detail: "nenhum runner GitHub registrado para este repo",
		})
		return out
	}
	for _, r := range runners {
		r.Repo = repo
		diag := ClassifyRunner(r)
		out.Runners = append(out.Runners, diag)
		out.Severity = maxSeverity(out.Severity, diag.Severity)
	}
	return out
}

func ClassifyRunner(r GitHubRunner) RunnerDiagnosis {
	diag := RunnerDiagnosis{
		ID: r.ID, Repo: r.Repo, Name: r.Name, Status: r.Status,
		Busy: r.Busy, Labels: append([]string(nil), r.Labels...),
		Severity: SeverityWarning,
	}
	hasCivm := hasLabel(r.Labels, "civm")
	switch {
	case r.Status == "online" && strings.HasPrefix(r.Name, "civm-") && hasCivm:
		diag.Classification = "canonical"
		diag.Severity = SeverityOK
		diag.Detail = "runner civm canonico online"
	case r.Status == "offline" && strings.HasPrefix(r.Name, "legacy-ci-"):
		diag.Classification = "legacy_stale"
		diag.Detail = "runner legacy legacy-ci offline; remover manualmente depois de confirmar que nao e usado"
		if r.ID != 0 && r.Repo != "" {
			diag.ManualRemoveCommand = fmt.Sprintf("gh api -X DELETE /repos/%s/actions/runners/%d", r.Repo, r.ID)
		}
	case r.Status == "online" && !hasCivm:
		diag.Classification = "ambiguous"
		diag.Detail = "runner online sem label civm; jobs runs-on [self-hosted, civm] nao devem depender dele"
	case r.Status == "online" && hasCivm:
		diag.Classification = "compatible"
		diag.Severity = SeverityOK
		diag.Detail = "runner online com label civm"
	case r.Status == "offline":
		diag.Classification = "offline"
		diag.Detail = "runner offline reportado; civmctl nao remove automaticamente"
	default:
		diag.Classification = "unknown"
		diag.Detail = "estado de runner nao reconhecido"
	}
	if r.Busy {
		diag.Severity = maxSeverity(diag.Severity, SeverityWarning)
		diag.Detail += "; busy=true"
	}
	return diag
}

func listGitHubRunners(ctx context.Context, repo string, runFn func(context.Context, string, ...string) ([]byte, error)) ([]GitHubRunner, error) {
	out, err := runFn(ctx, "gh", "api", fmt.Sprintf("/repos/%s/actions/runners", repo))
	if err != nil {
		return nil, fmt.Errorf("gh api actions/runners: %w", err)
	}
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
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh runners: %w", err)
	}
	runners := make([]GitHubRunner, 0, len(raw.Runners))
	for _, rr := range raw.Runners {
		item := GitHubRunner{ID: rr.ID, Repo: repo, Name: rr.Name, Status: rr.Status, Busy: rr.Busy}
		for _, label := range rr.Labels {
			item.Labels = append(item.Labels, label.Name)
		}
		runners = append(runners, item)
	}
	return runners, nil
}

func (r Report) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func (r Report) Render(w io.Writer) {
	fmt.Fprintf(w, "civm doctor | workflow=%s | exit=%d\n\n", r.WorkflowFile, r.Exit)
	fmt.Fprintln(w, "HOST")
	fmt.Fprintf(w, "%-16s %-10s %s\n", "CHECK", "SEVERITY", "DETAIL")
	for _, c := range r.HostChecks {
		fmt.Fprintf(w, "%-16s %-10s %s\n", c.Name, c.Severity, c.Detail)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "HOOKS")
	if len(r.HookChecks) == 0 {
		fmt.Fprintln(w, "  nenhum hook check coletado")
	} else {
		fmt.Fprintf(w, "%-20s %-10s %s\n", "CHECK", "SEVERITY", "DETAIL")
		for _, c := range r.HookChecks {
			fmt.Fprintf(w, "%-20s %-10s %s\n", c.Name, c.Severity, c.Detail)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "SYSTEMD RUNNERS")
	if len(r.SystemdRunners) == 0 {
		fmt.Fprintln(w, "  nenhum actions.runner.* encontrado")
	} else {
		fmt.Fprintf(w, "%-54s %-22s %-10s %s\n", "UNIT", "REPO", "STATE", "NAME")
		for _, s := range r.SystemdRunners {
			state := s.ActiveState
			if s.SubState != "" {
				state += "/" + s.SubState
			}
			fmt.Fprintf(w, "%-54s %-22s %-10s %s\n", truncate(s.UnitName, 54), truncate(s.Repo, 22), state, s.Name)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "GITHUB RUNNERS")
	fmt.Fprintf(w, "%-24s %-18s %-10s %-5s %-14s %-9s %s\n", "REPO", "NAME", "STATUS", "BUSY", "CLASS", "SEVERITY", "LABELS")
	var manual []string
	for _, repo := range r.GitHubRepos {
		for _, gr := range repo.Runners {
			fmt.Fprintf(w, "%-24s %-18s %-10s %-5t %-14s %-9s %s\n",
				truncate(repo.Repo, 24), truncate(gr.Name, 18), gr.Status, gr.Busy,
				gr.Classification, gr.Severity, strings.Join(gr.Labels, ","))
			if gr.ManualRemoveCommand != "" {
				manual = append(manual, gr.ManualRemoveCommand+" # "+gr.Name)
			}
		}
	}
	if len(manual) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "LEGACY OFFLINE CLEANUP (manual; civmctl nao executa)")
		for _, cmd := range manual {
			fmt.Fprintf(w, "  %s\n", cmd)
		}
	}
}

func severityFromHealth(s health.Status) Severity {
	switch s {
	case health.StatusOK:
		return SeverityOK
	case health.StatusWarn:
		return SeverityWarning
	default:
		return SeverityCritical
	}
}

func maxSeverity(a, b Severity) Severity {
	if b.ExitCode() > a.ExitCode() {
		return b
	}
	return a
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 2 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
