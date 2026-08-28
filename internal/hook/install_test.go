package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/civm"
)

// civmDeployDir mirrors the default deploy source dir used by the Execute path.
const civmDeployDir = civm.DefaultDeploySourceDir

func TestRenderInstallJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderInstallJSON(&buf, InstallResult{Executed: true, HooksDir: "/opt/civm/hooks"}); err != nil {
		t.Fatal(err)
	}
	var parsed InstallResult
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !parsed.Executed || parsed.HooksDir != "/opt/civm/hooks" {
		t.Fatalf("roundtrip: %+v", parsed)
	}
}

func TestRenderInstallText(t *testing.T) {
	var buf bytes.Buffer
	RenderInstallText(&buf, InstallResult{
		Executed:       true,
		HooksDir:       "/opt/civm/hooks",
		RunnerEnvFiles: []string{"/home/runner/actions-runner/.env"},
		Restarted:      true,
		Error:          "warn",
	})
	out := buf.String()
	for _, want := range []string{"EXECUTE", "/opt/civm/hooks", "/home/runner/actions-runner/.env", "restarted", "warn"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Fatalf("text missing %q:\n%s", want, out)
		}
	}
}

func TestRenderInstallTextDryRun(t *testing.T) {
	var buf bytes.Buffer
	RenderInstallText(&buf, InstallResult{Executed: false, HooksDir: "/opt/civm/hooks"})
	if !strings.Contains(buf.String(), "DRY-RUN") {
		t.Fatalf("expected DRY-RUN tag: %q", buf.String())
	}
}

func TestInstallDryRunSkipsWrites(t *testing.T) {
	var writes, removes []string
	opts := InstallOptions{
		Execute:    false,
		HooksDir:   "/opt/civm/hooks",
		RunnerGlob: "/home/*/actions-runner*",
		GlobFn:     func(string) ([]string, error) { return []string{"/home/runner/actions-runner"}, nil },
		ReadFileFn: func(string) ([]byte, error) { return nil, nil },
		WriteFileFn: func(p string, _ []byte, _ os.FileMode) error {
			writes = append(writes, p)
			return nil
		},
		MkdirAllFn: func(string, os.FileMode) error { return nil },
		RemoveFn:   func(p string) error { removes = append(removes, p); return nil },
	}
	res := Install(context.Background(), opts)
	if res.Error != "" {
		t.Fatalf("dry-run failed: %s", res.Error)
	}
	if len(writes) != 0 || len(removes) != 0 {
		t.Fatalf("dry-run had side effects: writes=%v removes=%v", writes, removes)
	}
	if len(res.RunnerEnvFiles) != 1 {
		t.Fatalf("expected 1 enumerated env file, got %v", res.RunnerEnvFiles)
	}
}

func TestInstallPropagatesMkdirError(t *testing.T) {
	opts := InstallOptions{
		Execute:    true,
		HooksDir:   "/opt/civm/hooks",
		MkdirAllFn: func(string, os.FileMode) error { return fmt.Errorf("EACCES") },
	}
	res := Install(context.Background(), opts)
	if res.Error == "" {
		t.Fatalf("expected mkdir error to surface")
	}
}

func TestInstallPropagatesGlobError(t *testing.T) {
	opts := InstallOptions{
		Execute:     true,
		HooksDir:    "/opt/civm/hooks",
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		WriteFileFn: func(string, []byte, os.FileMode) error { return nil },
		RemoveFn:    func(string) error { return os.ErrNotExist },
		GlobFn:      func(string) ([]string, error) { return nil, fmt.Errorf("pattern bad") },
	}
	res := Install(context.Background(), opts)
	if res.Error == "" {
		t.Fatalf("expected glob error to surface")
	}
}

func TestInstallRejectsRelativeMutationPaths(t *testing.T) {
	opts := DefaultInstallOptions()
	opts.Execute = true
	opts.HooksDir = "relative/hooks"
	res := Install(context.Background(), opts)
	if res.Error == "" || !strings.Contains(res.Error, "absolute") {
		t.Fatalf("expected absolute path error, got %q", res.Error)
	}

	opts = DefaultInstallOptions()
	opts.Execute = true
	opts.CivmctlPath = "bin/civmctl"
	res = Install(context.Background(), opts)
	if res.Error == "" || !strings.Contains(res.Error, "absolute") {
		t.Fatalf("expected absolute path error, got %q", res.Error)
	}
}

func TestInstallWritesHookScripts(t *testing.T) {
	writes := map[string]string{}
	perms := map[string]os.FileMode{}
	opts := InstallOptions{
		Execute:     true,
		HooksDir:    "/opt/civm/hooks",
		CivmctlPath: "/usr/local/bin/civmctl",
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		WriteFileFn: func(path string, data []byte, perm os.FileMode) error {
			writes[path] = string(data)
			perms[path] = perm
			return nil
		},
		RemoveFn:   func(string) error { return os.ErrNotExist },
		GlobFn:     func(string) ([]string, error) { return nil, nil },
		ReadFileFn: deployReadFileFn(nil),
		RunFn:      acceptVisudoRunFn(),
		RenameFn:   func(string, string) error { return nil },
	}
	res := Install(context.Background(), opts)
	if res.Error != "" {
		t.Fatalf("install error: %s", res.Error)
	}
	want := map[string]string{
		"/opt/civm/hooks/job-started.sh":   ScriptContent("/usr/local/bin/civmctl", EventJobStarted),
		"/opt/civm/hooks/job-completed.sh": ScriptContent("/usr/local/bin/civmctl", EventJobCompleted),
	}
	// The Execute path also writes the safedelete wrapper; assert hook scripts
	// got the expected perms but allow the extra wrapper write.
	for path, content := range want {
		if writes[path] != content {
			t.Fatalf("write %s = %q, want %q", path, writes[path], content)
		}
		if perms[path] != 0755 {
			t.Fatalf("perm %s = %v, want 0755", path, perms[path])
		}
	}
	if perms[civm.DefaultSafeDeleteWrapperPath] != 0755 {
		t.Fatalf("wrapper perm = %v, want 0755", perms[civm.DefaultSafeDeleteWrapperPath])
	}
}

func TestInstallReplacesExistingHookPaths(t *testing.T) {
	var removes []string
	writes := map[string]string{}
	opts := InstallOptions{
		Execute:     true,
		HooksDir:    "/opt/civm/hooks",
		CivmctlPath: "/usr/local/bin/civmctl",
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		WriteFileFn: func(path string, data []byte, _ os.FileMode) error {
			writes[path] = string(data)
			return nil
		},
		RemoveFn:   func(p string) error { removes = append(removes, p); return nil },
		GlobFn:     func(string) ([]string, error) { return nil, nil },
		ReadFileFn: deployReadFileFn(nil),
		RunFn:      acceptVisudoRunFn(),
		RenameFn:   func(string, string) error { return nil },
	}
	res := Install(context.Background(), opts)
	if res.Error != "" {
		t.Fatalf("install error: %s", res.Error)
	}
	for _, path := range []string{"/opt/civm/hooks/job-started", "/opt/civm/hooks/job-completed", "/opt/civm/hooks/job-started.sh", "/opt/civm/hooks/job-completed.sh"} {
		found := false
		for _, removed := range removes {
			if removed == path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected remove %s, removes=%v", path, removes)
		}
	}
	// 2 hook scripts + safedelete wrapper + sudoers temp staging file.
	if _, ok := writes["/opt/civm/hooks/job-started.sh"]; !ok {
		t.Fatalf("missing job-started.sh write: %v", writes)
	}
	if _, ok := writes["/opt/civm/hooks/job-completed.sh"]; !ok {
		t.Fatalf("missing job-completed.sh write: %v", writes)
	}
	if _, ok := writes[civm.DefaultSafeDeleteWrapperPath]; !ok {
		t.Fatalf("missing safedelete wrapper write: %v", writes)
	}
}

func TestInstallReplacesExistingShHooks(t *testing.T) {
	var removes []string
	opts := InstallOptions{
		Execute:     true,
		HooksDir:    "/opt/civm/hooks",
		CivmctlPath: "/usr/local/bin/civmctl",
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		WriteFileFn: func(string, []byte, os.FileMode) error { return nil },
		RemoveFn:    func(p string) error { removes = append(removes, p); return nil },
		GlobFn:      func(string) ([]string, error) { return nil, nil },
		ReadFileFn:  deployReadFileFn(nil),
		RunFn:       acceptVisudoRunFn(),
		RenameFn:    func(string, string) error { return nil },
	}
	Install(context.Background(), opts)
	wantLegacy := []string{
		"/opt/civm/hooks/job-started.sh",
		"/opt/civm/hooks/job-completed.sh",
	}
	for _, w := range wantLegacy {
		found := false
		for _, r := range removes {
			if r == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("legacy %s not cleaned (removes=%v)", w, removes)
		}
	}
}

func TestInstallSkipsUnsafeRunnerDirs(t *testing.T) {
	opts := DefaultInstallOptions()
	opts.Execute = false
	opts.GlobFn = func(string) ([]string, error) {
		return []string{"/etc/passwd", "/home/runner/actions-runner-x"}, nil
	}
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, nil }
	opts.WriteFileFn = func(string, []byte, os.FileMode) error { return nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	res := Install(context.Background(), opts)
	if len(res.RunnerEnvFiles) != 1 {
		t.Fatalf("expected unsafe path skipped, got %v", res.RunnerEnvFiles)
	}
	if !strings.HasPrefix(res.RunnerEnvFiles[0], "/home/runner/") {
		t.Fatalf("expected /home/runner path, got %q", res.RunnerEnvFiles[0])
	}
}

func TestInstallAllowsConfiguredRunnerRoot(t *testing.T) {
	opts := DefaultInstallOptions()
	opts.Execute = false
	opts.RunnerGlob = "/srv/ci/actions-runner*"
	opts.GlobFn = func(pattern string) ([]string, error) {
		if pattern != "/srv/ci/actions-runner*" {
			t.Fatalf("pattern = %q", pattern)
		}
		return []string{"/srv/ci/actions-runner-team"}, nil
	}
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, nil }
	opts.WriteFileFn = func(string, []byte, os.FileMode) error { return nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	res := Install(context.Background(), opts)
	if res.Error != "" {
		t.Fatalf("install error: %s", res.Error)
	}
	if len(res.RunnerEnvFiles) != 1 || res.RunnerEnvFiles[0] != "/srv/ci/actions-runner-team/.env" {
		t.Fatalf("runner env files = %v", res.RunnerEnvFiles)
	}
}

func TestRunnerDirCandidateRejectsSystemAndTemporaryRoots(t *testing.T) {
	for _, path := range []string{"/etc/actions-runner", "/usr/local/actions-runner", "/tmp/actions-runner", "actions-runner"} {
		if IsRunnerDirCandidate(path) {
			t.Fatalf("IsRunnerDirCandidate(%q) = true, want false", path)
		}
	}
}

func TestInstallUpsertsHookEnv(t *testing.T) {
	files := map[string][]byte{
		"/home/emdev/actions-runner/.env": []byte("FOO=bar\nACTIONS_RUNNER_HOOK_JOB_COMPLETED=/old\n"),
	}
	var writes []string
	opts := DefaultInstallOptions()
	opts.Execute = true
	opts.RestartRunners = false
	opts.GlobFn = func(string) ([]string, error) { return []string{"/home/emdev/actions-runner"}, nil }
	opts.AllocatePortFn = func(string) (int, error) { return 20000, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.ReadFileFn = deployReadFileFn(func(path string) ([]byte, error) { return files[path], nil })
	opts.RunFn = acceptVisudoRunFn()
	opts.RenameFn = func(string, string) error { return nil }
	opts.WriteFileFn = func(path string, data []byte, perm os.FileMode) error {
		writes = append(writes, path+"="+string(data))
		files[path] = data
		return nil
	}
	opts.RemoveFn = func(string) error { return os.ErrNotExist }
	res := Install(context.Background(), opts)
	if res.Error != "" {
		t.Fatalf("install error: %s", res.Error)
	}
	env := string(files["/home/emdev/actions-runner/.env"])
	for _, want := range []string{"FOO=bar", "ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/civm/hooks/job-started.sh", "ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/civm/hooks/job-completed.sh"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q:\n%s", want, env)
		}
	}
	if len(writes) < 1 {
		t.Fatalf("expected at least one env write, got %v", writes)
	}
}

// FuzzUpsertEnv enforces the idempotency invariant of upsertEnv: regardless
// of prior content, the resulting .env must contain exactly one well-formed
// ACTIONS_RUNNER_HOOK_JOB_STARTED= line and exactly one well-formed
// ACTIONS_RUNNER_HOOK_JOB_COMPLETED= line — and a second pass must produce
// the same bytes (idempotent).
func FuzzUpsertEnv(f *testing.F) {
	seeds := []string{
		"",
		"FOO=bar\n",
		"FOO=bar\nACTIONS_RUNNER_HOOK_JOB_COMPLETED=/old\n",
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=/a\nACTIONS_RUNNER_HOOK_JOB_STARTED=/b\n",
		"URL=http://a=b\n",
		"line-without-equals\n",
		"FOO=bar\r\nBAZ=qux\r\n",
		"\n\n\n",
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=/a\nACTIONS_RUNNER_HOOK_JOB_COMPLETED=/b\nFOO=keep\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	const envPath = "/tmp/fuzz.env"
	wantStarted := "ACTIONS_RUNNER_HOOK_JOB_STARTED=" + filepath.Join(defaultHooksDir, startedHookName)
	wantCompleted := "ACTIONS_RUNNER_HOOK_JOB_COMPLETED=" + filepath.Join(defaultHooksDir, completedHookName)

	f.Fuzz(func(t *testing.T, initial string) {
		files := map[string][]byte{envPath: []byte(initial)}
		opts := InstallOptions{
			HooksDir: defaultHooksDir,
			ReadFileFn: func(p string) ([]byte, error) {
				return files[p], nil
			},
			WriteFileFn: func(p string, data []byte, _ os.FileMode) error {
				files[p] = data
				return nil
			},
		}
		if err := upsertEnv(opts, envPath, nil); err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		first := string(files[envPath])
		if got := countPrefix(first, "ACTIONS_RUNNER_HOOK_JOB_STARTED="); got != 1 {
			t.Fatalf("first pass: started line count = %d, want 1\n%s", got, first)
		}
		if got := countPrefix(first, "ACTIONS_RUNNER_HOOK_JOB_COMPLETED="); got != 1 {
			t.Fatalf("first pass: completed line count = %d, want 1\n%s", got, first)
		}
		if !containsLine(first, wantStarted) {
			t.Fatalf("first pass: missing %q\n%s", wantStarted, first)
		}
		if !containsLine(first, wantCompleted) {
			t.Fatalf("first pass: missing %q\n%s", wantCompleted, first)
		}
		// Idempotency: a second pass must produce the same bytes.
		if err := upsertEnv(opts, envPath, nil); err != nil {
			t.Fatalf("second upsert: %v", err)
		}
		if got := string(files[envPath]); got != first {
			t.Fatalf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, got)
		}
	})
}

func TestRunnerSlot(t *testing.T) {
	cases := map[string]string{
		"/home/runner/actions-runner-cmpx": "cmpx",
		"/home/runner/actions-runner":      "actions-runner",
		"/srv/ci/my-runner":                "my-runner",
		"actions-runner-acme":              "acme",
	}
	for dir, want := range cases {
		if got := runnerSlot(dir); got != want {
			t.Fatalf("runnerSlot(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestUpsertEnvRejectsHookKeysInExtra(t *testing.T) {
	opts := InstallOptions{
		HooksDir:    defaultHooksDir,
		ReadFileFn:  func(string) ([]byte, error) { return nil, nil },
		WriteFileFn: func(string, []byte, os.FileMode) error { t.Fatal("write must not happen on rejection"); return nil },
	}
	err := upsertEnv(opts, "/tmp/x.env", map[string]string{"ACTIONS_RUNNER_HOOK_JOB_STARTED": "/evil"})
	if err == nil || !strings.Contains(err.Error(), "ACTIONS_RUNNER_HOOK_") {
		t.Fatalf("expected rejection of hook key in extra, got %v", err)
	}
}

func TestUpsertEnvWritesExtraDeterministically(t *testing.T) {
	files := map[string][]byte{"/tmp/x.env": []byte("FOO=bar\nCIVM_PORT_BASE=999\n")}
	opts := InstallOptions{
		HooksDir:    defaultHooksDir,
		ReadFileFn:  func(p string) ([]byte, error) { return files[p], nil },
		WriteFileFn: func(p string, d []byte, _ os.FileMode) error { files[p] = d; return nil },
	}
	extra := map[string]string{"CIVM_RUNNER_SLOT": "cmpx", "CIVM_PORT_BASE": "20000", "COMPOSE_PROJECT_NAME": "cmpx"}
	if err := upsertEnv(opts, "/tmp/x.env", extra); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	out := string(files["/tmp/x.env"])
	// Pre-existing FOO preserved; stale CIVM_PORT_BASE=999 stripped and replaced.
	for _, want := range []string{"FOO=bar", "CIVM_PORT_BASE=20000", "CIVM_RUNNER_SLOT=cmpx", "COMPOSE_PROJECT_NAME=cmpx"} {
		if !containsLine(out, want) {
			t.Fatalf("env missing %q:\n%s", want, out)
		}
	}
	if countPrefix(out, "CIVM_PORT_BASE=") != 1 {
		t.Fatalf("CIVM_PORT_BASE not deduped:\n%s", out)
	}
	// Extra keys are emitted in alphabetical order after the two hook lines.
	iSlot := strings.Index(out, "CIVM_PORT_BASE=")
	iName := strings.Index(out, "COMPOSE_PROJECT_NAME=")
	if iSlot < 0 || iName < 0 || iSlot > strings.Index(out, "CIVM_RUNNER_SLOT=") {
		t.Fatalf("extra keys not alphabetical:\n%s", out)
	}
	// Idempotency: a second pass yields identical bytes.
	first := out
	if err := upsertEnv(opts, "/tmp/x.env", extra); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if string(files["/tmp/x.env"]) != first {
		t.Fatalf("extra upsert not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, files["/tmp/x.env"])
	}
}

func TestInstallInjectsSlotAndPortBase(t *testing.T) {
	files := map[string][]byte{"/home/runner/actions-runner-cmpx/.env": []byte("EXISTING=1\n")}
	var gotSlot string
	opts := DefaultInstallOptions()
	opts.Execute = true
	opts.RestartRunners = false
	opts.GlobFn = func(string) ([]string, error) { return []string{"/home/runner/actions-runner-cmpx"}, nil }
	opts.AllocatePortFn = func(slot string) (int, error) { gotSlot = slot; return 20064, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.ReadFileFn = deployReadFileFn(func(p string) ([]byte, error) { return files[p], nil })
	opts.RunFn = acceptVisudoRunFn()
	opts.RenameFn = func(string, string) error { return nil }
	opts.WriteFileFn = func(p string, d []byte, _ os.FileMode) error { files[p] = d; return nil }
	opts.RemoveFn = func(string) error { return os.ErrNotExist }
	res := Install(context.Background(), opts)
	if res.Error != "" {
		t.Fatalf("install error: %s", res.Error)
	}
	if gotSlot != "cmpx" {
		t.Fatalf("AllocatePortFn slot = %q, want cmpx", gotSlot)
	}
	env := string(files["/home/runner/actions-runner-cmpx/.env"])
	for _, want := range []string{"EXISTING=1", "CIVM_RUNNER_SLOT=cmpx", "CIVM_PORT_BASE=20064", "COMPOSE_PROJECT_NAME=cmpx"} {
		if !containsLine(env, want) {
			t.Fatalf("env missing %q:\n%s", want, env)
		}
	}
}

func TestInstallSurfacesPortAllocationError(t *testing.T) {
	opts := DefaultInstallOptions()
	opts.Execute = true
	opts.RestartRunners = false
	opts.GlobFn = func(string) ([]string, error) { return []string{"/home/runner/actions-runner-cmpx"}, nil }
	opts.AllocatePortFn = func(string) (int, error) { return 0, fmt.Errorf("civm port window exhausted") }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.ReadFileFn = deployReadFileFn(nil)
	opts.RunFn = acceptVisudoRunFn()
	opts.RenameFn = func(string, string) error { return nil }
	opts.WriteFileFn = func(string, []byte, os.FileMode) error { return nil }
	opts.RemoveFn = func(string) error { return os.ErrNotExist }
	res := Install(context.Background(), opts)
	if res.Error == "" || !strings.Contains(res.Error, "allocate port block") {
		t.Fatalf("expected port allocation error to surface, got %q", res.Error)
	}
}

// Deploy-source fixtures shared by tests that exercise the Execute path, which
// now also installs the scoped sudoers wrapper. Content is opaque to these
// tests (the scoped-sudoers behavior is covered by scoped_sudoers_test.go); they
// only need ReadFileFn to return non-empty bytes and visudo to succeed.
const (
	fixtureWrapperContent = "#!/bin/sh\nexit 0\n"
	fixtureSudoersContent = "emdev ALL=(root) NOPASSWD: /usr/local/bin/civm-safedelete, /usr/local/bin/civm-generation-boundary\n"
)

// deployReadFileFn returns a ReadFileFn that serves the deploy wrapper + sudoers
// sources and delegates every other path to base (so callers keep their own
// .env/file fixtures). A nil base treats unknown paths as empty.
func deployReadFileFn(base func(string) ([]byte, error)) func(string) ([]byte, error) {
	wrapperSrc := filepath.Join(civmDeployDir, "bin/civm-safedelete")
	boundarySrc := filepath.Join(civmDeployDir, "bin/civm-generation-boundary")
	sudoersSrc := filepath.Join(civmDeployDir, "sudoers.d/civm-cleanup")
	return func(path string) ([]byte, error) {
		switch path {
		case wrapperSrc:
			return []byte(fixtureWrapperContent), nil
		case boundarySrc:
			return []byte(fixtureWrapperContent), nil
		case sudoersSrc:
			return []byte(fixtureSudoersContent), nil
		}
		if base != nil {
			return base(path)
		}
		return nil, nil
	}
}

func TestVerifySafeDeleteCapabilityFailsClosed(t *testing.T) {
	var gotArgs []string
	opts := InstallOptions{
		RunFn: func(_ context.Context, name string, args ...string) ([]byte, error) {
			gotArgs = append([]string{name}, args...)
			return nil, fmt.Errorf("sudo: a password is required")
		},
	}
	if err := verifySafeDeleteCapability(context.Background(), opts); err == nil {
		t.Fatal("expected fail-closed error when the --check probe fails")
	}
	want := []string{"sudo", "-n", civm.DefaultSafeDeleteWrapperPath, "--check"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("probe argv = %v, want %v", gotArgs, want)
	}
}

func TestVerifySafeDeleteCapabilityPassesOnZeroExit(t *testing.T) {
	opts := InstallOptions{
		RunFn: func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	}
	if err := verifySafeDeleteCapability(context.Background(), opts); err != nil {
		t.Fatalf("expected nil on a successful probe, got %v", err)
	}
}

func TestVerifyGenerationBoundaryCapabilityRequiresExactMarker(t *testing.T) {
	good := InstallOptions{
		RunFn: func(context.Context, string, ...string) ([]byte, error) {
			return []byte(civm.GenerationCleanBoundaryMarker + "\n"), nil
		},
	}
	if err := verifyGenerationBoundaryCapability(context.Background(), good); err != nil {
		t.Fatalf("expected exact marker to pass: %v", err)
	}

	bad := InstallOptions{
		RunFn: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("civm-generation-boundary/v0\n"), nil
		},
	}
	if err := verifyGenerationBoundaryCapability(context.Background(), bad); err == nil {
		t.Fatal("expected incompatible marker to fail closed")
	}
}

func TestInstallFailsClosedWhenCapabilityProbeFails(t *testing.T) {
	opts := InstallOptions{
		Execute:     true,
		HooksDir:    "/opt/civm/hooks",
		CivmctlPath: "/usr/local/bin/civmctl",
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		WriteFileFn: func(string, []byte, os.FileMode) error { return nil },
		RemoveFn:    func(string) error { return os.ErrNotExist },
		GlobFn:      func(string) ([]string, error) { return nil, nil },
		ReadFileFn:  deployReadFileFn(nil),
		RenameFn:    func(string, string) error { return nil },
		// visudo (and every other call) succeeds; ONLY the `sudo -n wrapper
		// --check` capability probe fails, so the install must fail closed.
		RunFn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			for _, a := range args {
				if a == "--check" {
					return nil, fmt.Errorf("NOPASSWD rule does not match")
				}
			}
			return nil, nil
		},
	}
	res := Install(context.Background(), opts)
	if res.Error == "" {
		t.Fatal("expected install to fail closed when the safedelete capability probe fails")
	}
	if !strings.Contains(res.Error, "capability probe") {
		t.Fatalf("error = %q, want mention of the capability probe", res.Error)
	}
}

// acceptVisudoRunFn returns a RunFn that accepts visudo validation and records
// nothing else, so Execute-path tests do not perform real sudo/visudo.
func acceptVisudoRunFn() func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == civm.DefaultGenerationBoundaryWrapperPath {
				return []byte(civm.GenerationCleanBoundaryMarker + "\n"), nil
			}
		}
		return nil, nil
	}
}

func countPrefix(content, prefix string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

func containsLine(content, want string) bool {
	for _, line := range strings.Split(content, "\n") {
		if line == want {
			return true
		}
	}
	return false
}
