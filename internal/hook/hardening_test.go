package hook

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/safedelete"
)

func clearRunnerEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
}

// TestJobCompletedCleanupFailureDoesNotFailJob is the regression guard for the
// 2026-06-10 incident: a green Web CI was marked failed because the
// job-completed hook exited 1 over a work_root leftover ("Complete runner").
// Post-job hygiene must stay visible (degraded decision + work_root action
// error feeding the runner-watchdog sentinel) but exit 0.
func TestJobCompletedCleanupFailureDoesNotFailJob(t *testing.T) {
	clearRunnerEnv(t)
	opts := DefaultOptionsFromEnv(EventJobCompleted)
	opts.Execute = true
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 60, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{fakeDirEntry("leftover")}, nil
	}
	opts.SafeWorkDeleteFn = func(context.Context, string) safedelete.Result {
		return safedelete.Result{Err: errors.New("unlinkat leftover: directory not empty")}
	}

	res := Run(context.Background(), opts)

	if res.ExitCode != 0 {
		t.Fatalf("job-completed cleanup failure must not fail the job: exit=%d res=%+v", res.ExitCode, res)
	}
	if res.Decision != DecisionCleanupDegraded {
		t.Fatalf("decision = %q, want %q", res.Decision, DecisionCleanupDegraded)
	}
	if res.Error == "" {
		t.Fatal("degraded result must keep the error visible in res.Error")
	}
	// The runner-watchdog broken-runner sentinel matches on the work_root
	// action error in hooks.jsonl — it must survive the demotion.
	var sentinel bool
	for _, a := range res.Actions {
		if a.Name == "work_root" && a.Error != "" {
			sentinel = true
		}
	}
	if !sentinel {
		t.Fatalf("work_root action error (watchdog sentinel) lost: %+v", res.Actions)
	}
}

// TestJobStartedCleanupFailureStillRejects is the positive pair: at job-started
// the cleanup runs BEFORE the job as a health gate, so a real (non-cache-race)
// failure must keep rejecting the job — only job-completed was demoted.
func TestJobStartedCleanupFailureStillRejects(t *testing.T) {
	clearRunnerEnv(t)
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 50
	opts.HardFailPct = 95
	// 80% used → disk-pressure cleanup runs at job-started.
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 20, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{fakeDirEntry("leftover")}, nil
	}
	opts.SafeWorkDeleteFn = func(context.Context, string) safedelete.Result {
		return safedelete.Result{Err: errors.New("escalation unavailable")}
	}

	res := Run(context.Background(), opts)

	if res.ExitCode == 0 {
		t.Fatalf("job-started cleanup failure must keep gating the job: res=%+v", res)
	}
	if res.Decision != DecisionError {
		t.Fatalf("decision = %q, want %q", res.Decision, DecisionError)
	}
}

// fakeDockerRunFn routes the docker subcommands killWorkRootContainers issues.
type fakeDockerRunFn struct {
	psOut      string
	psErr      error
	inspectOut string
	inspectErr error
	killed     [][]string
	killErr    error
	commands   []string
}

func (f *fakeDockerRunFn) fn(_ context.Context, name string, args ...string) ([]byte, error) {
	f.commands = append(f.commands, name+" "+strings.Join(args, " "))
	if name != "docker" || len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "ps":
		return []byte(f.psOut), f.psErr
	case "inspect":
		return []byte(f.inspectOut), f.inspectErr
	case "kill":
		f.killed = append(f.killed, args[1:])
		return nil, f.killErr
	}
	return nil, nil
}

func TestKillWorkRootContainersKillsOnlyOrphansUnderRoot(t *testing.T) {
	root := "/home/civm-test/actions-runner/_work"
	fake := &fakeDockerRunFn{
		psOut: "aaa\nbbb\nccc\nddd\n",
		// aaa: bind mount inside the root (orphan compose container).
		// bbb: sibling runner's root — must NOT be killed.
		// ccc: prefix collision (_work-evil) — must NOT be killed.
		// ddd: no mounts.
		inspectOut: strings.Join([]string{
			"aaa " + root + "/acme/app/web /var/lib/docker/volumes/x",
			"bbb /home/civm-test/actions-runner-other/_work/repo",
			"ccc " + root + "-evil/repo",
			"ddd",
		}, "\n"),
	}
	opts := Options{Execute: true, RunFn: fake.fn}

	a := killWorkRootContainers(context.Background(), opts, root)

	if a.Error != "" || a.Warning != "" {
		t.Fatalf("unexpected error/warning: %+v", a)
	}
	if len(fake.killed) != 1 {
		t.Fatalf("docker kill calls = %v, want exactly one", fake.killed)
	}
	if got := fake.killed[0]; len(got) != 1 || got[0] != "aaa" {
		t.Fatalf("killed %v, want only the orphan aaa", got)
	}
}

func TestKillWorkRootContainersNoContainersNoKill(t *testing.T) {
	fake := &fakeDockerRunFn{psOut: "\n"}
	opts := Options{Execute: true, RunFn: fake.fn}

	a := killWorkRootContainers(context.Background(), opts, "/home/civm-test/actions-runner/_work")

	if len(fake.killed) != 0 || a.Error != "" || a.Warning != "" {
		t.Fatalf("expected clean no-op: killed=%v action=%+v", fake.killed, a)
	}
}

func TestKillWorkRootContainersDockerDownIsWarningNeverFatal(t *testing.T) {
	fake := &fakeDockerRunFn{psErr: errors.New("docker daemon unreachable")}
	opts := Options{Execute: true, RunFn: fake.fn}

	a := killWorkRootContainers(context.Background(), opts, "/home/civm-test/actions-runner/_work")

	if a.Error != "" {
		t.Fatalf("docker being down must never be fatal: %+v", a)
	}
	if a.Warning == "" {
		t.Fatalf("docker being down must stay visible as a warning: %+v", a)
	}
}

// TestDegradedEnvNeverSweepsForeignRoots guards the 2026-06-10 cross-runner
// deletion: job-started hooks firing with a degraded env (empty
// RUNNER_TEMP/GITHUB_WORKSPACE) used to fall back to discovering ALL
// /home/*/actions-runner*/_work roots and — with no active workspace to
// protect — deleted a sibling runner's checkout mid-job. With no env-derived
// root the hook must clean NOTHING.
func TestDegradedEnvNeverSweepsForeignRoots(t *testing.T) {
	clearRunnerEnv(t)
	var removed []string
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 50
	opts.HardFailPct = 95
	// 80% used → disk-pressure cleanup runs; with the old global fallback this
	// would have swept every runner's root.
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 20, nil }
	opts.RemoveAllFn = func(p string) error { removed = append(removed, p); return nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.ReadDirFn = func(path string) ([]os.DirEntry, error) {
		t.Fatalf("hook with no env-derived work root must not read any root, got ReadDir(%q)", path)
		return nil, nil
	}
	opts.SafeWorkDeleteFn = func(_ context.Context, path string) safedelete.Result {
		t.Fatalf("hook with no env-derived work root must not delete anything, got %q", path)
		return safedelete.Result{}
	}

	res := Run(context.Background(), opts)

	for _, a := range res.Actions {
		if a.Name == "work_root" || a.Name == "docker_kill_workroot" {
			t.Fatalf("no work_root action may run without an env-derived root: %+v", a)
		}
	}
	if len(removed) != 0 {
		t.Fatalf("nothing may be removed: %v", removed)
	}
}

func TestKillWorkRootContainersDryRunDoesNothing(t *testing.T) {
	fake := &fakeDockerRunFn{psOut: "aaa\n"}
	opts := Options{Execute: false, RunFn: fake.fn}

	_ = killWorkRootContainers(context.Background(), opts, "/home/civm-test/actions-runner/_work")

	if len(fake.commands) != 0 {
		t.Fatalf("dry-run must not call docker: %v", fake.commands)
	}
}
