package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/health"
	"github.com/emersonbusson/civm/internal/hook"
	"github.com/emersonbusson/civm/internal/runner"
)

func TestClassifyRunner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		runner GitHubRunner
		class  string
		sev    Severity
	}{
		{
			name: "canonical",
			runner: GitHubRunner{Repo: "other/peer", Name: "civm-peer",
				Status: "online", Labels: []string{"self-hosted", "civm"}},
			class: "canonical", sev: SeverityOK,
		},
		{
			name: "legacy offline",
			runner: GitHubRunner{Repo: "other/peer", ID: 123, Name: "legacy-ci-1",
				Status: "offline", Labels: []string{"self-hosted", "legacy-ci"}},
			class: "legacy_stale", sev: SeverityWarning,
		},
		{
			name: "online without civm label",
			runner: GitHubRunner{Repo: "other/peer", Name: "custom",
				Status: "online", Labels: []string{"self-hosted"}},
			class: "ambiguous", sev: SeverityWarning,
		},
		{
			name: "busy canonical",
			runner: GitHubRunner{Repo: "other/peer", Name: "civm-peer",
				Status: "online", Busy: true, Labels: []string{"self-hosted", "civm"}},
			class: "canonical", sev: SeverityWarning,
		},
		{
			name: "compatible civm label with noncanonical name",
			runner: GitHubRunner{Repo: "other/peer", Name: "custom-vm",
				Status: "online", Labels: []string{"self-hosted", "civm"}},
			class: "compatible", sev: SeverityOK,
		},
		{
			name: "offline generic",
			runner: GitHubRunner{Repo: "other/peer", Name: "custom-vm",
				Status: "offline", Labels: []string{"self-hosted", "civm"}},
			class: "offline", sev: SeverityWarning,
		},
		{
			name: "unknown state",
			runner: GitHubRunner{Repo: "other/peer", Name: "custom-vm",
				Status: "draining", Labels: []string{"self-hosted", "civm"}},
			class: "unknown", sev: SeverityWarning,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyRunner(tt.runner)
			if got.Classification != tt.class || got.Severity != tt.sev {
				t.Fatalf("got class=%s severity=%s, want class=%s severity=%s", got.Classification, got.Severity, tt.class, tt.sev)
			}
			if tt.class == "legacy_stale" && !strings.Contains(got.ManualRemoveCommand, "/repos/other/peer/actions/runners/123") {
				t.Fatalf("manual remove command missing runner id: %+v", got)
			}
		})
	}
}

func TestListGitHubRunnersParsesLabels(t *testing.T) {
	t.Parallel()
	runFn := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "gh" {
			t.Fatalf("name = %q, want gh", name)
		}
		gotArgs := strings.Join(args, " ")
		if gotArgs != "api /repos/other/peer/actions/runners" {
			t.Fatalf("args = %q", gotArgs)
		}
		return []byte(`{
			"runners": [{
				"id": 123,
				"name": "civm-peer",
				"status": "online",
				"busy": true,
				"labels": [{"name": "self-hosted"}, {"name": "civm"}]
			}]
		}`), nil
	}

	got, err := listGitHubRunners(context.Background(), "other/peer", runFn)
	if err != nil {
		t.Fatalf("listGitHubRunners err = %v", err)
	}
	if len(got) != 1 || got[0].ID != 123 || got[0].Repo != "other/peer" || !got[0].Busy {
		t.Fatalf("runners = %+v", got)
	}
	if strings.Join(got[0].Labels, ",") != "self-hosted,civm" {
		t.Fatalf("labels = %+v", got[0].Labels)
	}
}

func TestListGitHubRunnersErrors(t *testing.T) {
	t.Parallel()
	_, err := listGitHubRunners(context.Background(), "other/peer",
		func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("network")
		})
	if err == nil || !strings.Contains(err.Error(), "gh api actions/runners") {
		t.Fatalf("run error = %v", err)
	}

	_, err = listGitHubRunners(context.Background(), "other/peer",
		func(context.Context, string, ...string) ([]byte, error) {
			return []byte(`{bad-json`), nil
		})
	if err == nil || !strings.Contains(err.Error(), "parse gh runners") {
		t.Fatalf("json error = %v", err)
	}
}

func TestClassifyRepoMissingAndUnknown(t *testing.T) {
	t.Parallel()
	missing := ClassifyRepo("acme/app", nil, nil)
	if missing.Severity != SeverityWarning || missing.Runners[0].Classification != "missing" {
		t.Fatalf("missing = %+v", missing)
	}
	unknown := ClassifyRepo("acme/app", nil, errors.New("gh auth"))
	if unknown.Severity != SeverityWarning || unknown.Runners[0].Classification != "unknown" {
		t.Fatalf("unknown = %+v", unknown)
	}
}

func TestCollectAndRenderJSON(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Repos = []string{"acme/civm", "other/peer"}
	opts.HealthFn = func(context.Context) health.Report {
		return health.Report{Checks: []health.Check{
			{Name: "DISK", Detail: "50 GB free", Status: health.StatusOK},
			{Name: "TIMER_DISK", Detail: "enabled+active", Status: health.StatusOK},
		}}
	}
	opts.SystemRunnersFn = func(context.Context) ([]runner.Status, error) {
		return []runner.Status{{UnitName: "actions.runner.other-peer.civm-peer.service", Repo: "other/peer", Name: "civm-peer", ActiveState: "active", SubState: "running"}}, nil
	}
	stubHookContractOK(&opts)
	opts.GitHubRunnersFn = func(_ context.Context, repo string) ([]GitHubRunner, error) {
		if repo == "acme/civm" {
			return []GitHubRunner{{Repo: repo, Name: "civm-self", Status: "online", Labels: []string{"self-hosted", "civm"}}}, nil
		}
		return []GitHubRunner{{Repo: repo, Name: "legacy-ci-1", Status: "offline", Labels: []string{"self-hosted", "legacy-ci"}}}, nil
	}
	report, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if report.Exit != 1 {
		t.Fatalf("Exit = %d, want warning 1", report.Exit)
	}
	var buf bytes.Buffer
	if err := report.RenderJSON(&buf); err != nil {
		t.Fatalf("RenderJSON err = %v", err)
	}
	var parsed Report
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(parsed.GitHubRepos) != 2 || parsed.GitHubRepos[1].Runners[0].Classification != "legacy_stale" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if len(parsed.HookChecks) != 8 {
		t.Fatalf("hook checks not rendered in JSON: %+v", parsed.HookChecks)
	}
}

func TestCollectReportsHookContractFailures(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Repos = []string{"acme/civm"}
	opts.HealthFn = func(context.Context) health.Report {
		return health.Report{Checks: []health.Check{
			{Name: "DISK", Detail: "50 GB free", Status: health.StatusOK},
		}}
	}
	opts.SystemRunnersFn = func(context.Context) ([]runner.Status, error) {
		return []runner.Status{
			{UnitName: "actions.runner.acme-civm.civm-self.service", Repo: "acme/civm", Name: "civm-self", ActiveState: "active", SubState: "running"},
			{UnitName: "actions.runner.other-peer.civm-peer.service", Repo: "other/peer", Name: "civm-peer", ActiveState: "inactive", SubState: "dead"},
		}, nil
	}
	opts.GitHubRunnersFn = func(_ context.Context, repo string) ([]GitHubRunner, error) {
		return []GitHubRunner{{Repo: repo, Name: "civm-self", Status: "online", Labels: []string{"self-hosted", "civm"}}}, nil
	}
	// Capability probe present, so the only criticals come from the hook
	// contract failures this test exercises (not from SCOPED_SUDOERS).
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, nil
	}
	// GlobFn isolado neste teste: só o glob de runner dirs retorna algo; o glob
	// de units-fonte (UNIT_SCRIPTS_INSTALLED) volta vazio para não introduzir um
	// achado fora do escopo das falhas de contrato de hook aqui exercidas.
	opts.GlobFn = func(pattern string) ([]string, error) {
		if strings.Contains(pattern, "civmctl-") {
			return nil, nil
		}
		return []string{"/home/emdev/actions-runner"}, nil
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		switch path {
		case "/opt/civm/hooks/job-started.sh":
			return []byte(hook.ScriptContent("/usr/local/bin/civmctl", hook.EventJobStarted)), nil
		case "/opt/civm/hooks/job-completed.sh":
			return []byte("#!/usr/bin/env bash\necho legacy\n"), nil
		case "/home/emdev/actions-runner/.env":
			return []byte(strings.Join([]string{
				"ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/civm/hooks/job-started",
				"ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/civm/hooks/job-completed.sh",
				"",
			}, "\n")), nil
		default:
			return nil, errors.New("unexpected read path: " + path)
		}
	}

	report, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if report.Exit != 2 {
		t.Fatalf("Exit = %d, want critical 2; hook_checks=%+v", report.Exit, report.HookChecks)
	}
	assertHookCheck(t, report, "HOOK_JOB_COMPLETED", SeverityCritical, "content mismatch")
	assertHookCheck(t, report, "HOOK_RUNNER_ENVS", SeverityCritical, "job-started")
	assertHookCheck(t, report, "RUNNER_SERVICES", SeverityCritical, "1/2")
}

// TestCollectFlagsRunnerSerializationCollision prova que o doctor detecta o
// runner org + por-repo coexistindo (o estado que quebrou o #1184) como crítico,
// com o resto do contrato verde para isolar a falha de serialização.
func TestCollectFlagsRunnerSerializationCollision(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Repos = []string{"acme/app"}
	opts.HealthFn = func(context.Context) health.Report {
		return health.Report{Checks: []health.Check{
			{Name: "DISK", Detail: "50 GB free", Status: health.StatusOK},
		}}
	}
	opts.SystemRunnersFn = func(context.Context) ([]runner.Status, error) {
		return []runner.Status{
			{UnitName: "actions.runner.acme.civm-acme-org.service", Repo: "acme", Name: "civm-acme-org", ActiveState: "active", SubState: "running"},
			{UnitName: "actions.runner.acme-app.civm-app.service", Repo: "acme/app", Name: "civm-app", ActiveState: "active", SubState: "running"},
		}, nil
	}
	stubHookContractOK(&opts)
	opts.GitHubRunnersFn = func(_ context.Context, repo string) ([]GitHubRunner, error) {
		return []GitHubRunner{{Repo: repo, Name: "civm-acme-org", Status: "online", Labels: []string{"self-hosted", "civm"}}}, nil
	}
	report, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	assertHookCheck(t, report, "RUNNER_SERIALIZATION", SeverityCritical, "civm-app")
	assertHookCheck(t, report, "RUNNER_SERIALIZATION", SeverityCritical, "civmctl runner remove")
	if report.Exit != 2 {
		t.Fatalf("Exit = %d, want critical 2 (serialização); hook_checks=%+v", report.Exit, report.HookChecks)
	}
}

// TestCollectRunnerSerializationOKWhenOrgOnly garante zero falso-positivo: só o
// runner org presente é o estado durável correto, severidade OK.
func TestCollectRunnerSerializationOKWhenOrgOnly(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Repos = []string{"acme/app"}
	opts.HealthFn = func(context.Context) health.Report {
		return health.Report{Checks: []health.Check{{Name: "DISK", Detail: "50 GB free", Status: health.StatusOK}}}
	}
	opts.SystemRunnersFn = func(context.Context) ([]runner.Status, error) {
		return []runner.Status{
			{UnitName: "actions.runner.acme.civm-acme-org.service", Repo: "acme", Name: "civm-acme-org", ActiveState: "active", SubState: "running"},
		}, nil
	}
	stubHookContractOK(&opts)
	opts.GitHubRunnersFn = func(_ context.Context, repo string) ([]GitHubRunner, error) {
		return []GitHubRunner{{Repo: repo, Name: "civm-acme-org", Status: "online", Labels: []string{"self-hosted", "civm"}}}, nil
	}
	report, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	assertHookCheck(t, report, "RUNNER_SERIALIZATION", SeverityOK, "sem runner por-repo redundante")
}

func TestCollectInfersReposFromSystemdWhenReposUnset(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Repos = nil
	opts.HealthFn = func(context.Context) health.Report {
		return health.Report{Checks: []health.Check{
			{Name: "DISK", Detail: "50 GB free", Status: health.StatusOK},
		}}
	}
	opts.SystemRunnersFn = func(context.Context) ([]runner.Status, error) {
		return []runner.Status{
			{UnitName: "actions.runner.acme-api.civm-api.service", Repo: "acme/api", Name: "civm-api", ActiveState: "active", SubState: "running"},
			{UnitName: "actions.runner.acme-web.civm-web.service", Repo: "acme/web", Name: "civm-web", ActiveState: "active", SubState: "running"},
			{UnitName: "actions.runner.acme-api.civm-api-2.service", Repo: "acme/api", Name: "civm-api-2", ActiveState: "active", SubState: "running"},
		}, nil
	}
	stubHookContractOK(&opts)
	var queried []string
	opts.GitHubRunnersFn = func(_ context.Context, repo string) ([]GitHubRunner, error) {
		queried = append(queried, repo)
		return []GitHubRunner{{Repo: repo, Name: "civm-auto", Status: "online", Labels: []string{"self-hosted", "civm"}}}, nil
	}
	report, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want ok 0; report=%+v", report.Exit, report)
	}
	if strings.Join(queried, ",") != "acme/api,acme/web" {
		t.Fatalf("queried repos = %v", queried)
	}
}

func TestRenderHumanIncludesManualLegacyCleanup(t *testing.T) {
	t.Parallel()
	r := Report{
		WorkflowFile: "ci.yml",
		HostChecks:   []HostCheck{{Name: "DISK", Severity: "ok", Detail: "50 GB free"}},
		GitHubRepos: []RepoDiagnosis{{
			Repo: "other/peer", Severity: SeverityWarning,
			Runners: []RunnerDiagnosis{{
				Repo: "other/peer", ID: 55, Name: "legacy-ci-1", Status: "offline",
				Classification: "legacy_stale", Severity: SeverityWarning,
				ManualRemoveCommand: "gh api -X DELETE /repos/other/peer/actions/runners/55",
			}},
		}},
		Exit: 1,
	}
	var buf bytes.Buffer
	r.Render(&buf)
	for _, want := range []string{"HOST", "GITHUB RUNNERS", "LEGACY OFFLINE CLEANUP", "gh api -X DELETE"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("Render omitted %q:\n%s", want, buf.String())
		}
	}
}

func TestCollectRejectsBadRepo(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Repos = []string{"bad/repo;rm"}
	if _, err := Collect(context.Background(), opts); err == nil {
		t.Fatalf("expected bad repo validation error")
	}
}

func TestCheckScopedSudoersCapabilityProbe(t *testing.T) {
	t.Parallel()

	// Capability present: the probe exits 0 -> SCOPED_SUDOERS OK. And it must
	// run the EXACT privileged invocation safedelete uses (sudo -n <wrapper>
	// --check), not read a file — existence is not function (DT-v2-10).
	var got string
	okOpts := DefaultOptions()
	okOpts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = strings.Join(append([]string{name}, args...), " ")
		return nil, nil
	}
	if c := checkScopedSudoers(context.Background(), okOpts); c.Severity != SeverityOK {
		t.Fatalf("probe success should be OK, got %+v", c)
	}
	want := "sudo -n " + civm.DefaultSafeDeleteWrapperPath + " --check"
	if got != want {
		t.Fatalf("probe invocation = %q, want %q", got, want)
	}

	// Pair the positive with the refusal: capability gone (sudo -n fails) ->
	// Critical. A success-only test could hide a probe that never fails.
	failOpts := DefaultOptions()
	failOpts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("sudo: a password is required")
	}
	if c := checkScopedSudoers(context.Background(), failOpts); c.Severity != SeverityCritical {
		t.Fatalf("probe failure should be Critical, got %+v", c)
	}
}

func TestCheckGenerationBoundaryCapabilityRequiresExactMarker(t *testing.T) {
	t.Parallel()

	var got string
	okOpts := DefaultOptions()
	okOpts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = strings.Join(append([]string{name}, args...), " ")
		return []byte(civm.GenerationCleanBoundaryMarker + "\n"), nil
	}
	if c := checkGenerationBoundaryCapability(context.Background(), okOpts); c.Severity != SeverityOK {
		t.Fatalf("exact marker should be OK, got %+v", c)
	}
	want := "sudo -n " + civm.DefaultGenerationBoundaryWrapperPath + " --check"
	if got != want {
		t.Fatalf("probe invocation = %q, want %q", got, want)
	}

	badOpts := DefaultOptions()
	badOpts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("civm-generation-boundary/v0\n"), nil
	}
	if c := checkGenerationBoundaryCapability(context.Background(), badOpts); c.Severity != SeverityCritical {
		t.Fatalf("incompatible marker should be Critical, got %+v", c)
	}
}

func TestCheckAdmitCgroup(t *testing.T) {
	t.Parallel()
	// cgroup v2 with memory controller → OK.
	okOpts := DefaultOptions()
	okOpts.ReadFileFn = func(path string) ([]byte, error) {
		if path == cgroupControllersPath {
			return []byte("cpuset cpu io memory pids\n"), nil
		}
		return nil, errors.New("unexpected: " + path)
	}
	if c := checkAdmitCgroup(okOpts); c.Severity != string(SeverityOK) {
		t.Fatalf("cgroup v2 + memory should be OK, got %+v", c)
	}
	// cgroup v2 present but no memory controller → WARN (degraded, DT-v3-6).
	noMem := DefaultOptions()
	noMem.ReadFileFn = func(string) ([]byte, error) { return []byte("cpuset cpu io pids\n"), nil }
	if c := checkAdmitCgroup(noMem); c.Severity != string(SeverityWarning) {
		t.Fatalf("missing memory controller should WARN, got %+v", c)
	}
	// No cgroup2 file at all → WARN (admit degrades to watchdog-gated, DT-v3-6).
	absent := DefaultOptions()
	absent.ReadFileFn = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if c := checkAdmitCgroup(absent); c.Severity != string(SeverityWarning) {
		t.Fatalf("absent cgroup2 should WARN (degraded), got %+v", c)
	}
}

func TestCheckAdmitRunAsUser(t *testing.T) {
	t.Parallel()
	// systemd-run --pipe -p MemoryMax runs as the runner user (NOT root) → OK,
	// and it must use a SERVICE (--pipe), never --scope (DT-v3-1).
	var got string
	okOpts := DefaultOptions()
	okOpts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = strings.Join(append([]string{name}, args...), " ")
		return []byte("emdev\n"), nil
	}
	c := checkAdmitRunAsUser(context.Background(), okOpts, "emdev")
	if c.Severity != string(SeverityOK) {
		t.Fatalf("run-as-user emdev should be OK, got %+v", c)
	}
	if strings.Contains(got, "--scope") {
		t.Fatalf("probe must not use --scope (runs as root): %q", got)
	}
	if !strings.Contains(got, "--pipe") || !strings.Contains(got, "MemoryMax=") {
		t.Fatalf("probe must use --pipe -p MemoryMax: %q", got)
	}

	// Probe returns root → CRITICAL (the v2 footgun this whole SPEC fixes).
	rootOpts := DefaultOptions()
	rootOpts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return []byte("root\n"), nil }
	if c := checkAdmitRunAsUser(context.Background(), rootOpts, "emdev"); c.Severity != string(SeverityCritical) {
		t.Fatalf("run-as root should be CRITICAL, got %+v", c)
	}

	// Probe command fails (no NOPASSWD / no systemd-run) → WARN (capability gone).
	failOpts := DefaultOptions()
	failOpts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("sudo: password required")
	}
	if c := checkAdmitRunAsUser(context.Background(), failOpts, "emdev"); c.Severity != string(SeverityWarning) {
		t.Fatalf("probe failure should WARN, got %+v", c)
	}
}

func TestCheckAdmitRAMInvariant(t *testing.T) {
	t.Parallel()
	// MaxHeavy × effMB <= MemTotal − host holds → OK. 16GB total, 2GB host,
	// generous effMB=(16384-2048)/2=7168, 2×7168=14336 <= 14336. OK.
	okOpts := DefaultOptions()
	okOpts.ReadFileFn = func(path string) ([]byte, error) {
		if path == meminfoPath {
			return []byte("MemTotal:       16777216 kB\nMemAvailable:    8388608 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"), nil
		}
		return nil, errors.New("unexpected: " + path)
	}
	if c := checkAdmitRAMInvariant(okOpts); c.Severity != string(SeverityOK) {
		t.Fatalf("RAM invariant should hold for 16GB host, got %+v", c)
	}
	// Meminfo unreadable → WARN (cannot verify; never crashes doctor).
	badOpts := DefaultOptions()
	badOpts.ReadFileFn = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if c := checkAdmitRAMInvariant(badOpts); c.Severity != string(SeverityWarning) {
		t.Fatalf("unreadable meminfo should WARN, got %+v", c)
	}
}

func TestCollectIncludesAdmitHostChecks(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Repos = []string{"acme/civm"}
	opts.HealthFn = func(context.Context) health.Report {
		return health.Report{Checks: []health.Check{{Name: "DISK", Detail: "ok", Status: health.StatusOK}}}
	}
	opts.SystemRunnersFn = func(context.Context) ([]runner.Status, error) {
		return []runner.Status{{UnitName: "actions.runner.acme-civm.civm-self.service", Repo: "acme/civm", Name: "civm-self", ActiveState: "active", SubState: "running"}}, nil
	}
	opts.GitHubRunnersFn = func(_ context.Context, repo string) ([]GitHubRunner, error) {
		return []GitHubRunner{{Repo: repo, Name: "civm-self", Status: "online", Labels: []string{"self-hosted", "civm"}}}, nil
	}
	stubHookContractOK(&opts)
	// stubHookContractOK only stubs the hook paths; extend ReadFileFn for the
	// admit probes (cgroup + meminfo) so Collect exercises the admit checks.
	base := opts.ReadFileFn
	opts.ReadFileFn = func(path string) ([]byte, error) {
		switch path {
		case cgroupControllersPath:
			return []byte("cpu memory pids\n"), nil
		case meminfoPath:
			return []byte("MemTotal: 16777216 kB\nMemAvailable: 8388608 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"), nil
		default:
			return base(path)
		}
	}
	opts.RunFn = stubRunFnWithRequestedUser

	report, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	names := map[string]bool{}
	for _, c := range report.HostChecks {
		names[c.Name] = true
	}
	for _, want := range []string{"ADMIT_CGROUP", "ADMIT_RUN_AS_USER", "ADMIT_RAM_INVARIANT"} {
		if !names[want] {
			t.Fatalf("missing admit host check %q in %+v", want, report.HostChecks)
		}
	}
}

// TestExtractUnitScriptsPicksOnlyShTargets prova que a extração pega o token
// /usr/local/bin/*.sh tanto em ExecStart (envolto por flock) quanto em
// ConditionPathExists, deduplica, e ignora units que apontam só para o binário
// civmctl (sem .sh).
func TestExtractUnitScriptsPicksOnlyShTargets(t *testing.T) {
	t.Parallel()
	withScript := "[Unit]\nConditionPathExists=/usr/local/bin/civm-ci-artifact-prune.sh\n" +
		"[Service]\nExecStart=/usr/bin/flock -n /run/x.lock /usr/local/bin/civm-ci-artifact-prune.sh\n"
	got := extractUnitScripts(withScript)
	if len(got) != 1 || got[0] != "/usr/local/bin/civm-ci-artifact-prune.sh" {
		t.Fatalf("extractUnitScripts = %v, want one civm-ci-artifact-prune.sh", got)
	}

	binaryOnly := "[Unit]\nConditionPathExists=/usr/local/bin/civmctl\n" +
		"[Service]\nExecStart=/usr/local/bin/civmctl cleanup --execute\n"
	if got := extractUnitScripts(binaryOnly); len(got) != 0 {
		t.Fatalf("binary-only unit should yield no .sh scripts, got %v", got)
	}
}

// TestCheckUnitScriptsInstalled é o par positivo+negativo (Kahneman #13): uma
// unit cujo script /usr/local/bin/*.sh está ausente vira finding CRÍTICO; a
// mesma unit com o script presente fica OK. Isso prova o EFEITO — o doctor
// pega de fato o gap que faz o systemd pular a unit em silêncio, não só que o
// check existe.
func TestCheckUnitScriptsInstalled(t *testing.T) {
	t.Parallel()
	const (
		unitPath   = "/opt/civm/deploy/systemd/civmctl-registry-cache.service"
		scriptPath = "/usr/local/bin/setup-registry-cache.sh"
	)
	unitContent := []byte("[Unit]\nConditionPathExists=" + scriptPath +
		"\n[Service]\nExecStart=" + scriptPath + "\n")

	baseOpts := func() Options {
		o := DefaultOptions()
		o.UnitsSourceDir = "/opt/civm/deploy/systemd"
		o.GlobFn = func(pattern string) ([]string, error) {
			if !strings.Contains(pattern, "civmctl-") {
				t.Fatalf("unexpected glob pattern %q", pattern)
			}
			return []string{unitPath}, nil
		}
		o.ReadFileFn = func(path string) ([]byte, error) {
			if path == unitPath {
				return unitContent, nil
			}
			return nil, errors.New("unexpected read: " + path)
		}
		return o
	}

	// Negativo: script ausente -> CRÍTICO, com o caminho e a unit na mensagem.
	missing := baseOpts()
	missing.StatFn = func(path string) (os.FileInfo, error) {
		if path == scriptPath {
			return nil, os.ErrNotExist
		}
		t.Fatalf("unexpected stat: %s", path)
		return nil, nil
	}
	gotMissing := checkUnitScriptsInstalled(missing)
	if gotMissing.Severity != SeverityCritical {
		t.Fatalf("missing script should be Critical, got %+v", gotMissing)
	}
	for _, want := range []string{scriptPath, "civmctl-registry-cache.service"} {
		if !strings.Contains(gotMissing.Detail, want) {
			t.Fatalf("missing-script detail %q omits %q", gotMissing.Detail, want)
		}
	}

	// Positivo: mesmo unit, script presente -> OK (sem finding).
	present := baseOpts()
	present.StatFn = func(path string) (os.FileInfo, error) {
		if path == scriptPath {
			return nil, nil
		}
		t.Fatalf("unexpected stat: %s", path)
		return nil, nil
	}
	gotPresent := checkUnitScriptsInstalled(present)
	if gotPresent.Severity != SeverityOK {
		t.Fatalf("present script should be OK, got %+v", gotPresent)
	}
}

func assertHookCheck(t *testing.T, report Report, name string, severity Severity, detailContains string) {
	t.Helper()
	for _, check := range report.HookChecks {
		if check.Name == name {
			if check.Severity != severity || !strings.Contains(check.Detail, detailContains) {
				t.Fatalf("%s = %+v, want severity=%s detail containing %q", name, check, severity, detailContains)
			}
			return
		}
	}
	t.Fatalf("missing hook check %s in %+v", name, report.HookChecks)
}

// stubUnitPath é a unit-fonte sintética que stubHookContractOK serve para o
// check UNIT_SCRIPTS_INSTALLED, e stubUnitScript é o script .sh que ela
// referencia. StatFn reporta esse script como presente para manter o check OK.
const (
	stubUnitPath   = "/opt/civm/deploy/systemd/civmctl-buildcache-prune.service"
	stubUnitScript = "/usr/local/bin/civm-ci-artifact-prune.sh"
)

func stubHookContractOK(opts *Options) {
	opts.RunFn = stubRunFnWithRequestedUser
	// GlobFn agora atende dois padrões: o glob de runner dirs (HOOK_RUNNER_ENVS)
	// e o glob de units-fonte (UNIT_SCRIPTS_INSTALLED). Dispatch pelo padrão.
	opts.GlobFn = func(pattern string) ([]string, error) {
		if strings.Contains(pattern, "civmctl-") {
			return []string{stubUnitPath}, nil
		}
		return []string{"/home/emdev/actions-runner"}, nil
	}
	// StatFn reporta o script da unit sintética como instalado (check verde).
	opts.StatFn = func(path string) (os.FileInfo, error) {
		if path == stubUnitScript {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	opts.ReadFileFn = func(path string) ([]byte, error) {
		switch path {
		case "/opt/civm/hooks/job-started.sh":
			return []byte(hook.ScriptContent("/usr/local/bin/civmctl", hook.EventJobStarted)), nil
		case "/opt/civm/hooks/job-completed.sh":
			return []byte(hook.ScriptContent("/usr/local/bin/civmctl", hook.EventJobCompleted)), nil
		case "/home/emdev/actions-runner/.env":
			return []byte(strings.Join([]string{
				"ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/civm/hooks/job-started.sh",
				"ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/civm/hooks/job-completed.sh",
				"",
			}, "\n")), nil
		case stubUnitPath:
			return []byte("[Unit]\nConditionPathExists=" + stubUnitScript +
				"\n[Service]\nExecStart=/usr/bin/flock -n /run/x.lock " + stubUnitScript + "\n"), nil
		case cgroupControllersPath:
			return []byte("cpu memory pids\n"), nil
		case meminfoPath:
			return []byte("MemTotal: 16777216 kB\nMemAvailable: 8388608 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"), nil
		default:
			return nil, errors.New("unexpected read path: " + path)
		}
	}
}

func stubRunFnWithRequestedUser(_ context.Context, _ string, args ...string) ([]byte, error) {
	for _, arg := range args {
		if arg == civm.DefaultGenerationBoundaryWrapperPath {
			return []byte(civm.GenerationCleanBoundaryMarker + "\n"), nil
		}
	}
	for i, arg := range args {
		if arg == "-p" && i+1 < len(args) && strings.HasPrefix(args[i+1], "User=") {
			return []byte(strings.TrimPrefix(args[i+1], "User=") + "\n"), nil
		}
	}
	return []byte("ok\n"), nil
}
