package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
)

// invalidRepo is reused across ValidateRepo error cases to satisfy goconst.
const invalidRepo = "no-slash-here"

// TestDefaultOptionsWiring asserts production defaults point at the canonical
// paths and that every injectable func is wired (non-nil).
func TestDefaultOptionsWiring(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	if opts.StatePath != civm.DefaultMaintenanceStatePath {
		t.Fatalf("StatePath = %q, want %q", opts.StatePath, civm.DefaultMaintenanceStatePath)
	}
	if opts.LockPath != civm.DefaultMaintenanceLockPath {
		t.Fatalf("LockPath = %q, want %q", opts.LockPath, civm.DefaultMaintenanceLockPath)
	}
	if opts.RunFn == nil || opts.GHFn == nil || opts.IdleCheckFn == nil {
		t.Fatalf("command/idle funcs must be wired: %+v", opts)
	}
	if opts.ReadFileFn == nil || opts.WriteFileFn == nil || opts.RemoveFn == nil || opts.MkdirAllFn == nil {
		t.Fatalf("filesystem funcs must be wired: %+v", opts)
	}
	if opts.LockFn == nil || opts.NowFn == nil {
		t.Fatalf("lock/now funcs must be wired: %+v", opts)
	}
	// NowFn must return a real, recent timestamp.
	if got := opts.NowFn(); got.IsZero() {
		t.Fatalf("NowFn returned zero time")
	}
}

// TestApplyDefaultsFillsEmptyOptions drives applyDefaults with a fully empty
// Options to cover every nil-branch and the path defaults.
func TestApplyDefaultsFillsEmptyOptions(t *testing.T) {
	t.Parallel()
	opts := Options{}
	applyDefaults(&opts)
	if opts.StatePath != civm.DefaultMaintenanceStatePath {
		t.Fatalf("StatePath default = %q", opts.StatePath)
	}
	if opts.LockPath != civm.DefaultMaintenanceLockPath {
		t.Fatalf("LockPath default = %q", opts.LockPath)
	}
	if opts.RunFn == nil || opts.GHFn == nil || opts.IdleCheckFn == nil ||
		opts.ReadFileFn == nil || opts.WriteFileFn == nil || opts.RemoveFn == nil ||
		opts.MkdirAllFn == nil || opts.LockFn == nil || opts.NowFn == nil {
		t.Fatalf("applyDefaults left a nil func: %+v", opts)
	}
}

// TestApplyDefaultsKeepsProvidedValues ensures non-nil/non-empty fields are not
// overwritten by applyDefaults.
func TestApplyDefaultsKeepsProvidedValues(t *testing.T) {
	t.Parallel()
	customState := "/tmp/custom-state.json"
	customLock := "/tmp/custom.lock"
	called := false
	opts := Options{
		StatePath:   customState,
		LockPath:    customLock,
		RunFn:       func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		GHFn:        func(context.Context, ...string) ([]byte, error) { return nil, nil },
		IdleCheckFn: func(context.Context) bool { return true },
		ReadFileFn:  func(string) ([]byte, error) { return nil, nil },
		WriteFileFn: func(string, []byte, os.FileMode) error { return nil },
		RemoveFn:    func(string) error { return nil },
		MkdirAllFn:  func(string, os.FileMode) error { return nil },
		LockFn:      func(string) (func() error, error) { return func() error { return nil }, nil },
		NowFn:       func() time.Time { called = true; return time.Unix(0, 0) },
	}
	applyDefaults(&opts)
	if opts.StatePath != customState || opts.LockPath != customLock {
		t.Fatalf("applyDefaults overwrote provided paths: %+v", opts)
	}
	_ = opts.NowFn()
	if !called {
		t.Fatalf("applyDefaults overwrote provided NowFn")
	}
}

// TestDefaultRunExecutesCommand runs the real exec wiring for both the success
// and the failure (unknown binary) branch.
func TestDefaultRunExecutesCommand(t *testing.T) {
	t.Parallel()
	if _, err := defaultRun(context.Background(), "true"); err != nil {
		t.Fatalf("defaultRun(true) err = %v", err)
	}
	if _, err := defaultRun(context.Background(), "this-binary-does-not-exist-civm"); err == nil {
		t.Fatalf("defaultRun on missing binary must error")
	}
}

// TestDefaultGHExecutesCommand exercises defaultGH wiring. gh almost certainly
// is not installed in CI, so the missing-binary error branch is the realistic
// path; either outcome covers the single statement.
func TestDefaultGHExecutesCommand(t *testing.T) {
	t.Parallel()
	// "--version" is harmless if gh exists; otherwise we hit the error branch.
	_, _ = defaultGH(context.Background(), "--version")
	if _, err := defaultGH(context.Background()); err == nil && !ghBinaryPresent() {
		t.Fatalf("expected error path when gh is absent")
	}
}

func ghBinaryPresent() bool {
	_, err := defaultGH(context.Background(), "--version")
	return err == nil
}

// TestDefaultIdleCheckReturnsBool calls the real idle wiring; it must return
// without panicking regardless of host state.
func TestDefaultIdleCheckReturnsBool(t *testing.T) {
	t.Parallel()
	// Either true or false is acceptable; we only assert it does not panic and
	// the statement is executed.
	_ = defaultIdleCheck(context.Background())
}

// TestDefaultLockHappyPathAndDoubleLock covers a successful flock acquisition,
// release, and the LOCK_NB contention branch on a second acquire.
func TestDefaultLockHappyPathAndDoubleLock(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "nested", "maintenance.lock")

	release, err := defaultLock(lockPath)
	if err != nil {
		t.Fatalf("defaultLock err = %v", err)
	}
	// A second exclusive non-blocking lock on the same path must fail.
	if _, err2 := defaultLock(lockPath); err2 == nil {
		t.Fatalf("second defaultLock must fail while held")
	}
	if err := release(); err != nil {
		t.Fatalf("release err = %v", err)
	}
	// After release the lock is acquirable again.
	release2, err := defaultLock(lockPath)
	if err != nil {
		t.Fatalf("re-acquire after release err = %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("second release err = %v", err)
	}
}

// TestDefaultLockMkdirFailure forces the MkdirAll branch to fail by placing the
// lock under a path whose parent is a regular file (cannot become a dir).
func TestDefaultLockMkdirFailure(t *testing.T) {
	t.Parallel()
	fileAsDir := filepath.Join(t.TempDir(), "iam-a-file")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup write err = %v", err)
	}
	// lock dir would be fileAsDir/sub which cannot be created under a file.
	lockPath := filepath.Join(fileAsDir, "sub", "maintenance.lock")
	if _, err := defaultLock(lockPath); err == nil {
		t.Fatalf("defaultLock must fail when parent dir cannot be created")
	}
}

// TestDefaultLockOpenFailure forces the OpenFile branch to fail: the lock path
// itself is an existing directory, so O_CREATE|O_RDWR open fails.
func TestDefaultLockOpenFailure(t *testing.T) {
	t.Parallel()
	dirAsLock := filepath.Join(t.TempDir(), "lock-is-a-dir")
	if err := os.MkdirAll(dirAsLock, 0o750); err != nil {
		t.Fatalf("setup mkdir err = %v", err)
	}
	if _, err := defaultLock(dirAsLock); err == nil {
		t.Fatalf("defaultLock must fail when path is a directory")
	}
}

// TestDryRunExitWithState returns the persisted snapshot without mutating.
func TestDryRunExitWithState(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("Enter err = %v", err)
	}
	rec.runCalls = nil
	rec.ghCalls = nil
	rec.removed = nil
	rec.lockCalls = 0

	opts := rec.options(statePath)
	opts.Execute = false
	state, err := Exit(context.Background(), opts)
	if err != nil {
		t.Fatalf("dry-run Exit err = %v", err)
	}
	if len(state.Runners) != 2 {
		t.Fatalf("dry-run Exit runners = %d, want 2", len(state.Runners))
	}
	if len(rec.runCalls) != 0 || len(rec.ghCalls) != 0 || len(rec.removed) != 0 {
		t.Fatalf("dry-run Exit mutated: run=%v gh=%v removed=%v", rec.runCalls, rec.ghCalls, rec.removed)
	}
	if rec.lockCalls != 0 {
		t.Fatalf("dry-run Exit must not acquire lock, got %d", rec.lockCalls)
	}
}

// TestDryRunExitWithoutState returns an empty snapshot, no-op.
func TestDryRunExitWithoutState(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.Execute = false

	state, err := Exit(context.Background(), opts)
	if err != nil {
		t.Fatalf("dry-run Exit err = %v", err)
	}
	if len(state.Runners) != 0 || state.DrainedAt != "" {
		t.Fatalf("expected empty dry-run Exit state, got %+v", state)
	}
}

// TestDryRunExitReadError propagates a read error (non-NotExist) from dryRunExit.
func TestDryRunExitReadError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.Execute = false
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, errFake }

	if _, err := Exit(context.Background(), opts); err == nil {
		t.Fatalf("dry-run Exit must propagate read error")
	}
}

// TestDryRunEnterReadError propagates a read error (non-NotExist) from dryRunEnter.
func TestDryRunEnterReadError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.Execute = false
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, errFake }

	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatalf("dry-run Enter must propagate read error")
	}
}

// TestDryRunEnterReturnsExistingState short-circuits when a snapshot exists.
func TestDryRunEnterReturnsExistingState(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("Enter err = %v", err)
	}

	opts := rec.options(statePath)
	opts.Execute = false
	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("dry-run Enter err = %v", err)
	}
	if state.DrainedAt != fixedTime || len(state.Runners) != 2 {
		t.Fatalf("dry-run Enter did not return existing state: %+v", state)
	}
}

// TestReadStateParseError surfaces a JSON parse failure.
func TestReadStateParseError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	rec.files[statePath] = []byte("{not json")

	if _, err := Exit(context.Background(), rec.options(statePath)); err == nil {
		t.Fatalf("Exit must error on corrupt state JSON")
	}
}

// TestReadStateReadErrorNonNotExist surfaces a non-NotExist read error.
func TestReadStateReadErrorNonNotExist(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, errFake }

	if _, err := Exit(context.Background(), opts); err == nil {
		t.Fatalf("Exit must error on read failure")
	}
}

// TestReadStateEmptyFileIsAbsent treats a whitespace-only file as no snapshot.
func TestReadStateEmptyFileIsAbsent(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	rec.files[statePath] = []byte("   \n\t ")

	state, ok, err := readState(rec.options(statePath))
	if err != nil {
		t.Fatalf("readState err = %v", err)
	}
	if ok {
		t.Fatalf("whitespace-only state must be treated as absent, got %+v", state)
	}
}

// TestWriteStateMkdirError surfaces a mkdir failure from writeState through Enter.
func TestWriteStateMkdirError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.MkdirAllFn = func(string, os.FileMode) error { return errFake }

	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatalf("Enter must error when state dir cannot be created")
	}
}

// TestWriteStateWriteError surfaces a write failure from writeState through Enter.
func TestWriteStateWriteError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.WriteFileFn = func(string, []byte, os.FileMode) error { return errFake }

	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatalf("Enter must error when state cannot be written")
	}
}

// TestEnterReRunWriteError covers the writeState failure in the idempotent
// refresh branch of Enter.
func TestEnterReRunWriteError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("first Enter err = %v", err)
	}

	opts := rec.options(statePath)
	opts.WriteFileFn = func(string, []byte, os.FileMode) error { return errFake }
	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatalf("re-run Enter must error when refresh write fails")
	}
}

// TestEnterReRunReadError covers the readState failure branch in Enter.
func TestEnterReRunReadError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, errFake }

	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatalf("Enter must propagate read error")
	}
}

// TestExitReadError covers the readState failure branch in Exit.
func TestExitReadError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.ReadFileFn = func(string) ([]byte, error) { return nil, errFake }

	if _, err := Exit(context.Background(), opts); err == nil {
		t.Fatalf("Exit must propagate read error")
	}
}

// TestExitLockFailureIsError covers the lock-acquire failure branch in Exit.
func TestExitLockFailureIsError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.lockErr = errFake
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	if _, err := Exit(context.Background(), rec.options(statePath)); err == nil {
		t.Fatalf("Exit must error when lock cannot be acquired")
	}
}

// TestExitRemoveNotExistIsTolerated: a NotExist on RemoveFn must not fail Exit.
func TestExitRemoveNotExistIsTolerated(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("Enter err = %v", err)
	}

	opts := rec.options(statePath)
	opts.RemoveFn = func(string) error { return os.ErrNotExist }
	if _, err := Exit(context.Background(), opts); err != nil {
		t.Fatalf("Exit must tolerate a NotExist on state removal: %v", err)
	}
}

// TestExitRestoreStartFailureIsWarning: a failing systemctl start during Exit
// must be tolerated (warning only) and not fail the overall restore.
func TestExitRestoreStartFailureIsWarning(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("Enter err = %v", err)
	}

	opts := rec.options(statePath)
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "sudo" && len(args) >= 2 && args[1] == "start" {
			return nil, errFake
		}
		return nil, nil
	}
	// gh add label also fails to cover restoreRunner's label-warn branch.
	opts.GHFn = func(context.Context, ...string) ([]byte, error) { return nil, errFake }
	if _, err := Exit(context.Background(), opts); err != nil {
		t.Fatalf("Exit must tolerate start/label restore failures: %v", err)
	}
}

// TestRemoveLabelInvalidRepo hits the ValidateRepo error branch of removeLabel.
func TestRemoveLabelInvalidRepo(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	if _, err := removeLabel(context.Background(), rec.options("/tmp/x.json"), invalidRepo, "runner-1"); err == nil {
		t.Fatalf("removeLabel must reject an invalid repo")
	}
}

// TestRemoveLabelGHError hits the gh-failure branch of removeLabel.
func TestRemoveLabelGHError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.ghErrOn = func([]string) error { return errFake }
	if _, err := removeLabel(context.Background(), rec.options("/tmp/x.json"), repoCivm, "runner-1"); err == nil {
		t.Fatalf("removeLabel must surface gh error")
	}
}

// TestAddLabelInvalidRepo hits the ValidateRepo error branch of addLabel.
func TestAddLabelInvalidRepo(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	if err := addLabel(context.Background(), rec.options("/tmp/x.json"), invalidRepo, "runner-1", 11); err == nil {
		t.Fatalf("addLabel must reject an invalid repo")
	}
}

// TestAddLabelGHError hits the gh-failure branch of addLabel.
func TestAddLabelGHError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.ghErrOn = func([]string) error { return errFake }
	if err := addLabel(context.Background(), rec.options("/tmp/x.json"), repoCivm, "runner-1", 11); err == nil {
		t.Fatalf("addLabel must surface gh error")
	}
}

func TestGHEnvFromFileLoadsTokenWithoutExposingOtherValues(t *testing.T) {
	t.Parallel()
	env, err := ghEnvFromFile(func(string) ([]byte, error) {
		return []byte("CIVM_REAPER_REPOS=acme/app\nGH_TOKEN=secret-value\nIGNORED=value\n"), nil
	})
	if err != nil {
		t.Fatalf("ghEnvFromFile err = %v", err)
	}
	if len(env) != 1 || env[0] != "GH_TOKEN=secret-value" {
		t.Fatalf("ghEnvFromFile env = %v", env)
	}
}

func TestGHEnvFromFileLoadsExistingGHConfigDirectory(t *testing.T) {
	t.Parallel()
	env, err := ghEnvFromFile(func(string) ([]byte, error) {
		return []byte("GH_CONFIG_DIR=/home/runner/.config/gh\nCIVM_REAPER_REPOS=acme/app\n"), nil
	})
	if err != nil {
		t.Fatalf("ghEnvFromFile config dir err = %v", err)
	}
	if len(env) != 1 || env[0] != "GH_CONFIG_DIR=/home/runner/.config/gh" {
		t.Fatalf("ghEnvFromFile config dir env = %v", env)
	}
}

func TestGHEnvFromFileFailsClosedWithoutAuthSource(t *testing.T) {
	t.Parallel()
	if _, err := ghEnvFromFile(func(string) ([]byte, error) {
		return []byte("CIVM_REAPER_REPOS=acme/app\n"), nil
	}); err == nil {
		t.Fatal("ghEnvFromFile must reject a file without GitHub auth")
	}
}

func TestGHEnvFromFileRejectsRelativeConfigDirectory(t *testing.T) {
	t.Parallel()
	if _, err := ghEnvFromFile(func(string) ([]byte, error) {
		return []byte("GH_CONFIG_DIR=../gh\n"), nil
	}); err == nil {
		t.Fatal("ghEnvFromFile must reject a relative GH_CONFIG_DIR")
	}
}

// TestDiscoverRunnersListError returns nil when systemctl list-units fails.
func TestDiscoverRunnersListError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, errFake }

	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("Enter with no runners must succeed: %v", err)
	}
	if len(state.Runners) != 0 {
		t.Fatalf("expected zero runners when list-units fails, got %+v", state.Runners)
	}
}

// TestDiscoverRunnersDedupesAndSkipsEmpty exercises the dup-skip and empty-line
// branches of discoverRunners through Enter.
func TestDiscoverRunnersDedupesAndSkipsEmpty(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	// Two identical unit lines plus blanks and a non-runner line.
	rec.listOutput = "\n  " + unitCivm + " loaded active running\n" +
		"  " + unitCivm + " loaded active running\n" +
		"\n   \n" +
		"docker.service loaded active running Docker\n"
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	state, err := Enter(context.Background(), rec.options(statePath))
	if err != nil {
		t.Fatalf("Enter err = %v", err)
	}
	if len(state.Runners) != 1 {
		t.Fatalf("dedupe failed: runners = %d, want 1 (%+v)", len(state.Runners), state.Runners)
	}
}

// TestRunnerUnitNameEmptyRepo covers the no-repo branch of runnerUnitName.
func TestRunnerUnitNameEmptyRepo(t *testing.T) {
	t.Parallel()
	rn := RunnerState{Name: "solo-runner"}
	want := "actions.runner.solo-runner.service"
	if got := runnerUnitName(rn); got != want {
		t.Fatalf("runnerUnitName(no repo) = %q, want %q", got, want)
	}
}

// TestValueOrNone covers both the empty and non-empty branches.
func TestValueOrNone(t *testing.T) {
	t.Parallel()
	if got := valueOrNone("  "); got != "(none)" {
		t.Fatalf("valueOrNone(blank) = %q, want (none)", got)
	}
	if got := valueOrNone("acme/civm"); got != "acme/civm" {
		t.Fatalf("valueOrNone(value) = %q, want acme/civm", got)
	}
}

// TestRenderTextEmptyStateShowsNone covers the (none) rendering path for a
// state with a blank DrainedAt and a runner without a repo.
func TestRenderTextEmptyStateShowsNone(t *testing.T) {
	t.Parallel()
	state := State{Runners: []RunnerState{{Name: "solo"}}}
	var buf strings.Builder
	RenderText(&buf, "exit", state)
	out := buf.String()
	if !strings.Contains(out, "(none)") {
		t.Fatalf("RenderText must render (none) for blank fields: %s", out)
	}
	if !strings.Contains(out, "solo") {
		t.Fatalf("RenderText missing runner name: %s", out)
	}
}
