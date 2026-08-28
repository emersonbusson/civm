package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/portblock"
)

const (
	defaultHooksDir   = "/opt/civm/hooks"
	defaultCivmctlBin = "/usr/local/bin/civmctl"
	defaultRunnerGlob = "/home/*/actions-runner*"
	startedHookName   = "job-started.sh"
	completedHookName = "job-completed.sh"

	DefaultHooksDir   = defaultHooksDir
	DefaultCivmctlBin = defaultCivmctlBin
	DefaultRunnerGlob = defaultRunnerGlob
	StartedHookName   = startedHookName
	CompletedHookName = completedHookName
)

type InstallOptions struct {
	Execute        bool
	HooksDir       string
	CivmctlPath    string // binary invoked by job-started.sh / job-completed.sh scripts
	RunnerGlob     string
	RestartRunners bool
	GlobFn         func(pattern string) ([]string, error)
	ReadFileFn     func(path string) ([]byte, error)
	WriteFileFn    func(path string, data []byte, perm os.FileMode) error
	MkdirAllFn     func(path string, perm os.FileMode) error
	RemoveFn       func(path string) error // remove one file or symlink, never recursively
	RunFn          func(ctx context.Context, name string, args ...string) ([]byte, error)
	// RenameFn atomically activates the sudoers drop-in (temp -> final) only
	// after visudo validates it. Injected so unit tests never touch real /etc.
	// Defaults to os.Rename.
	RenameFn func(oldpath, newpath string) error
	// DeploySourceDir is the root from which the scoped sudoers + safedelete
	// wrapper are read at --execute time. Defaults to civm.DefaultDeploySourceDir.
	// Mirrors DefaultUnitsSourceDir / ScriptContent: a single source of truth in
	// deploy/, never go:embed (forbidden across the package boundary, DT-v2-5).
	DeploySourceDir string
	// SkipScopedSudoers disables installing the privileged safedelete wrapper +
	// scoped sudoers drop-in. Provisioning (`hook install --execute`,
	// `bootstrap-everything`) leaves it false so the capability is installed. The
	// periodic runner watchdog sets it true: its job is light-touch .env repair,
	// not re-running visudo and rewriting /etc/sudoers.d on every timer tick.
	SkipScopedSudoers bool
	// AllocatePortFn returns the sticky CIVM_PORT_BASE for a runner slot. It is
	// injected so unit tests never touch the real /var/lib/civm/port-blocks.json
	// state file; the default wraps portblock.Allocate.
	AllocatePortFn func(slot string) (int, error)
}

type InstallResult struct {
	Executed       bool     `json:"executed"`
	HooksDir       string   `json:"hooks_dir"`
	RunnerEnvFiles []string `json:"runner_env_files"`
	Restarted      bool     `json:"restarted"`
	Error          string   `json:"error,omitempty"`
}

func DefaultInstallOptions() InstallOptions {
	return InstallOptions{
		HooksDir:        defaultHooksDir,
		CivmctlPath:     defaultCivmctlBin,
		RunnerGlob:      defaultRunnerGlob,
		DeploySourceDir: civm.DefaultDeploySourceDir,
		GlobFn:          filepath.Glob,
		ReadFileFn:      os.ReadFile,
		WriteFileFn:     os.WriteFile,
		MkdirAllFn:      os.MkdirAll,
		RemoveFn:        os.Remove,
		RunFn:           defaultRun,
		RenameFn:        os.Rename,
		AllocatePortFn:  defaultAllocatePort,
		RestartRunners:  true,
	}
}

// defaultAllocatePort assigns (and persists) the sticky port block for a slot
// using the production portblock state file.
func defaultAllocatePort(slot string) (int, error) {
	return portblock.Allocate(portblock.DefaultOptions(), slot)
}

// runnerSlot derives the CI project slot from a runner directory name (DT-v2-12):
// "actions-runner-cmpx" -> "cmpx"; "actions-runner" -> "actions-runner";
// "my-runner" -> "my-runner". No realpath resolution.
func runnerSlot(dir string) string {
	b := filepath.Base(dir)
	if s := strings.TrimPrefix(b, "actions-runner-"); s != b && s != "" {
		return s
	}
	return b
}

func Install(ctx context.Context, opts InstallOptions) InstallResult {
	applyInstallDefaults(&opts)
	res := InstallResult{Executed: opts.Execute, HooksDir: opts.HooksDir}
	if err := validateInstallOptions(opts); err != nil {
		return installError(res, err)
	}
	if opts.Execute {
		if err := opts.MkdirAllFn(opts.HooksDir, 0755); err != nil {
			return installError(res, err)
		}
		// Clean up an invalid transition: the runner requires .sh, .ps1 or
		// .js suffixes in ACTIONS_RUNNER_HOOK_* paths.
		for _, legacy := range []string{"job-started", "job-completed"} {
			path := filepath.Join(opts.HooksDir, legacy)
			if err := opts.RemoveFn(path); err != nil && !os.IsNotExist(err) {
				return installError(res, err)
			}
		}
		hooks := []struct {
			name  string
			event Event
		}{
			{startedHookName, EventJobStarted},
			{completedHookName, EventJobCompleted},
		}
		for _, item := range hooks {
			path := filepath.Join(opts.HooksDir, item.name)
			if err := ensureHookScript(opts, path, item.event); err != nil {
				return installError(res, err)
			}
		}
		// Install the privileged fixed-command wrappers and scoped sudoers drop-in
		// (docs/specs/civm-runner-reliability, DT-v2-1/3/5/7/8). Idempotent and
		// fail-closed: the sudoers is only activated after visudo accepts it. The
		// periodic watchdog opts out (SkipScopedSudoers) — provisioning owns it.
		if !opts.SkipScopedSudoers {
			if err := installScopedSudoers(ctx, opts); err != nil {
				return installError(res, err)
			}
			// Functional capability probe (audit CRITICAL #2 / DT-v2-1/14): the
			// visudo check above only validates SYNTAX. Prove the escalation will
			// actually work as root before declaring install successful.
			if err := verifySafeDeleteCapability(ctx, opts); err != nil {
				return installError(res, err)
			}
			if err := verifyGenerationBoundaryCapability(ctx, opts); err != nil {
				return installError(res, err)
			}
		}
	}
	runners, err := opts.GlobFn(opts.RunnerGlob)
	if err != nil {
		return installError(res, err)
	}
	sort.Strings(runners)
	for _, runner := range runners {
		if !safeRunnerDir(runner) {
			continue
		}
		envPath := filepath.Join(runner, ".env")
		res.RunnerEnvFiles = append(res.RunnerEnvFiles, envPath)
		if opts.Execute {
			slot := runnerSlot(runner)
			base, err := opts.AllocatePortFn(slot)
			if err != nil {
				return installError(res, fmt.Errorf("allocate port block for slot %s: %w", slot, err))
			}
			extra := map[string]string{
				"CIVM_RUNNER_SLOT":     slot,
				"CIVM_PORT_BASE":       strconv.Itoa(base),
				"COMPOSE_PROJECT_NAME": slot,
			}
			if err := upsertEnv(opts, envPath, extra); err != nil {
				return installError(res, err)
			}
		}
	}
	if opts.Execute && opts.RestartRunners {
		if _, err := opts.RunFn(ctx, "systemctl", "restart", "actions.runner.*"); err != nil {
			return installError(res, err)
		}
		res.Restarted = true
	}
	return res
}

// verifySafeDeleteCapability runs the privileged wrapper's no-op capability probe
// through sudo. It is the FUNCTIONAL counterpart to the visudo SYNTAX check in
// installScopedSudoers: installing the wrapper + sudoers drop-in proves nothing
// until `sudo -n <wrapper> --check` actually returns 0. A secure_path mismatch, a
// wrong sudoers user, or a non-invokable wrapper passes visudo yet wedges the
// runner at "Complete runner" in production. Fail-closed here (audit CRITICAL #2,
// DT-v2-1/14) so a broken escalation never ships silently behind a green install.
func verifySafeDeleteCapability(ctx context.Context, opts InstallOptions) error {
	if _, err := opts.RunFn(ctx, "sudo", "-n", civm.DefaultSafeDeleteWrapperPath, "--check"); err != nil {
		return fmt.Errorf(
			"safedelete capability probe failed (sudo -n %s --check): %w; the NOPASSWD "+
				"rule does not match or the wrapper is not invokable as root",
			civm.DefaultSafeDeleteWrapperPath, err)
	}
	return nil
}

// verifyGenerationBoundaryCapability proves that the host-side protocol has
// exactly the version the C# controller accepts. It is a no-op wrapper probe;
// prepare/resume remain unavailable unless both sudoers and civmctl match.
func verifyGenerationBoundaryCapability(ctx context.Context, opts InstallOptions) error {
	out, err := opts.RunFn(ctx, "sudo", "-n", civm.DefaultGenerationBoundaryWrapperPath, "--check")
	if err != nil {
		return fmt.Errorf(
			"generation-boundary capability probe failed (sudo -n %s --check): %w; the NOPASSWD "+
				"rule does not match, the wrapper is not invokable, or civmctl is incompatible",
			civm.DefaultGenerationBoundaryWrapperPath, err)
	}
	if strings.TrimSpace(string(out)) != civm.GenerationCleanBoundaryMarker {
		return fmt.Errorf(
			"generation-boundary capability probe returned incompatible marker %q (want %q)",
			strings.TrimSpace(string(out)), civm.GenerationCleanBoundaryMarker)
	}
	return nil
}

func validateInstallOptions(opts InstallOptions) error {
	if strings.ContainsRune(opts.HooksDir, 0) || !filepath.IsAbs(filepath.Clean(opts.HooksDir)) {
		return fmt.Errorf("hooks-dir must be an absolute path")
	}
	if strings.ContainsRune(opts.CivmctlPath, 0) || !filepath.IsAbs(filepath.Clean(opts.CivmctlPath)) {
		return fmt.Errorf("civmctl-path must be an absolute path")
	}
	if strings.ContainsRune(opts.RunnerGlob, 0) {
		return fmt.Errorf("runner-glob contains NUL byte")
	}
	return nil
}

// upsertEnv reconciles the runner .env so it always carries exactly one of each
// civm-managed hook line plus the caller-provided extra keys. The two
// ACTIONS_RUNNER_HOOK_* lines are owned by this function, so extra must not try
// to set them (DT-v2-8). Existing managed/extra keys are stripped first, then
// the two hooks are reappended, then the extra keys in deterministic alphabetical
// order — making repeated installs idempotent.
func upsertEnv(opts InstallOptions, envPath string, extra map[string]string) error {
	for k := range extra {
		if strings.HasPrefix(k, "ACTIONS_RUNNER_HOOK_") {
			return fmt.Errorf("extra must not contain ACTIONS_RUNNER_HOOK_* keys")
		}
	}
	data, err := opts.ReadFileFn(envPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(data), "\n")
	var kept []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key := line
		if i := strings.IndexByte(line, '='); i >= 0 {
			key = line[:i]
		}
		if key == "ACTIONS_RUNNER_HOOK_JOB_STARTED" || key == "ACTIONS_RUNNER_HOOK_JOB_COMPLETED" {
			continue
		}
		if _, isExtra := extra[key]; isExtra {
			continue
		}
		kept = append(kept, line)
	}
	kept = append(kept,
		"ACTIONS_RUNNER_HOOK_JOB_STARTED="+filepath.Join(opts.HooksDir, startedHookName),
		"ACTIONS_RUNNER_HOOK_JOB_COMPLETED="+filepath.Join(opts.HooksDir, completedHookName),
	)
	extraKeys := make([]string, 0, len(extra))
	for k := range extra {
		extraKeys = append(extraKeys, k)
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		kept = append(kept, k+"="+extra[k])
	}
	return opts.WriteFileFn(envPath, []byte(strings.Join(kept, "\n")+"\n"), 0644)
}

func ensureHookScript(opts InstallOptions, path string, event Event) error {
	if err := opts.RemoveFn(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing %s: %w", path, err)
	}
	if err := opts.WriteFileFn(path, []byte(ScriptContent(opts.CivmctlPath, event)), 0755); err != nil {
		return fmt.Errorf("write hook script %s: %w", path, err)
	}
	return nil
}

func ScriptContent(civmctlPath string, event Event) string {
	return fmt.Sprintf("#!/usr/bin/env bash\nset -euo pipefail\nexec %s hook %s --execute \"$@\"\n", shellQuote(civmctlPath), event)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// IsRunnerDirCandidate returns true for absolute GitHub runner directories
// that are safe for hook .env reconciliation.
func IsRunnerDirCandidate(path string) bool {
	if strings.ContainsRune(path, 0) {
		return false
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || path == string(os.PathSeparator) {
		return false
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "actions-runner") {
		return false
	}
	sep := string(os.PathSeparator)
	blockedRoots := []string{"/bin", "/boot", "/dev", "/etc", "/proc", "/run", "/sys", "/tmp", "/usr", "/var/tmp"}
	for _, root := range blockedRoots {
		if path == root || strings.HasPrefix(path, root+sep) {
			return false
		}
	}
	return true
}

func safeRunnerDir(path string) bool {
	return IsRunnerDirCandidate(path)
}

func installError(res InstallResult, err error) InstallResult {
	if err != nil {
		res.Error = err.Error()
	}
	return res
}

func RenderInstallJSON(w io.Writer, r InstallResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func RenderInstallText(w io.Writer, r InstallResult) {
	mode := "DRY-RUN"
	if r.Executed {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "civm hook install: %s\nHooks dir: %s\n", mode, r.HooksDir)
	for _, env := range r.RunnerEnvFiles {
		fmt.Fprintf(w, "  env %s\n", env)
	}
	if r.Restarted {
		fmt.Fprintln(w, "Runners restarted")
	}
	if r.Error != "" {
		fmt.Fprintf(w, "Error: %s\n", r.Error)
	}
}

func applyInstallDefaults(opts *InstallOptions) {
	if opts.HooksDir == "" {
		opts.HooksDir = defaultHooksDir
	}
	if opts.CivmctlPath == "" {
		opts.CivmctlPath = defaultCivmctlBin
	}
	if opts.RunnerGlob == "" {
		opts.RunnerGlob = defaultRunnerGlob
	}
	if opts.GlobFn == nil {
		opts.GlobFn = filepath.Glob
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
	if opts.RemoveFn == nil {
		opts.RemoveFn = os.Remove
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.RenameFn == nil {
		opts.RenameFn = os.Rename
	}
	if opts.DeploySourceDir == "" {
		opts.DeploySourceDir = civm.DefaultDeploySourceDir
	}
	if opts.AllocatePortFn == nil {
		opts.AllocatePortFn = defaultAllocatePort
	}
}
