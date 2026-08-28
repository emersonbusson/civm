package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	repoCivm  = "acme/civm"
	repoVitae = "other/peer"
	orgAcme  = "acme"

	unitCivm  = "actions.runner.acme-civm.runner-1.service"
	unitVitae = "actions.runner.other-peer.runner-2.service"
	unitOrg   = "actions.runner.acme.civm-acme-org.service"

	fixedTime = "2026-05-29T12:00:00Z"
)

var errFake = errors.New("fake failure")

// recorder captures injected side effects so tests assert behaviour without
// touching systemctl, gh, the network or the real filesystem.
type recorder struct {
	runCalls   [][]string
	ghCalls    [][]string
	idle       bool
	runErrOn   func(name string, args []string) error
	ghErrOn    func(args []string) error
	listOutput string

	files     map[string][]byte
	removed   []string
	lockCalls int
	lockErr   error
}

func newRecorder() *recorder {
	return &recorder{idle: true, files: map[string][]byte{}}
}

func (r *recorder) options(statePath string) Options {
	return Options{
		Execute:   true,
		StatePath: statePath,
		LockPath:  filepath.Join(filepath.Dir(statePath), "maintenance.lock"),
		RunFn: func(_ context.Context, name string, args ...string) ([]byte, error) {
			r.runCalls = append(r.runCalls, append([]string{name}, args...))
			if strings.Contains(strings.Join(args, " "), "list-units") {
				return []byte(r.listOutput), nil
			}
			if r.runErrOn != nil {
				if err := r.runErrOn(name, args); err != nil {
					return nil, err
				}
			}
			return nil, nil
		},
		GHFn: func(_ context.Context, args ...string) ([]byte, error) {
			r.ghCalls = append(r.ghCalls, append([]string{"gh"}, args...))
			if r.ghErrOn != nil {
				if err := r.ghErrOn(args); err != nil {
					return nil, err
				}
			}
			if strings.Contains(strings.Join(args, " "), "actions/runners?per_page=100") {
				return []byte(`[{"runners":[{"id":11,"name":"runner-1"},{"id":12,"name":"runner-2"},{"id":71,"name":"civm-acme-org"}]}]`), nil
			}
			return nil, nil
		},
		IdleCheckFn: func(context.Context) bool { return r.idle },
		ReadFileFn: func(path string) ([]byte, error) {
			data, ok := r.files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return data, nil
		},
		WriteFileFn: func(path string, data []byte, _ os.FileMode) error {
			r.files[path] = append([]byte(nil), data...)
			return nil
		},
		RemoveFn: func(path string) error {
			r.removed = append(r.removed, path)
			delete(r.files, path)
			return nil
		},
		MkdirAllFn: func(string, os.FileMode) error { return nil },
		LockFn: func(string) (func() error, error) {
			r.lockCalls++
			if r.lockErr != nil {
				return nil, r.lockErr
			}
			return func() error { return nil }, nil
		},
		NowFn: func() time.Time { return mustTime(fixedTime) },
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func twoRunnerListing() string {
	return "  " + unitCivm + "  loaded active running GitHub Actions Runner\n" +
		"● " + unitVitae + "  loaded failed failed GitHub Actions Runner\n"
}

func runnerByName(state State, name string) (RunnerState, bool) {
	for _, rn := range state.Runners {
		if rn.Name == name {
			return rn, true
		}
	}
	return RunnerState{}, false
}

func hasRunCall(calls [][]string, want string) bool {
	for _, call := range calls {
		if strings.Join(call, " ") == want {
			return true
		}
	}
	return false
}

func TestEnterWritesStateAndDrainsBothRunners(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	state, err := Enter(context.Background(), rec.options(statePath))
	if err != nil {
		t.Fatalf("Enter err = %v", err)
	}
	if state.DrainedAt != fixedTime {
		t.Fatalf("DrainedAt = %q, want %q", state.DrainedAt, fixedTime)
	}
	if len(state.Runners) != 2 {
		t.Fatalf("Runners = %d, want 2", len(state.Runners))
	}
	for _, name := range []string{"runner-1", "runner-2"} {
		rn, ok := runnerByName(state, name)
		if !ok {
			t.Fatalf("missing runner %q in state", name)
		}
		if !rn.Stopped || !rn.LabelRemoved {
			t.Fatalf("runner %q not fully drained: %+v", name, rn)
		}
	}
	// State must be persisted to disk.
	if _, ok := rec.files[statePath]; !ok {
		t.Fatalf("state file not written to %s", statePath)
	}
	// Exactly one DELETE label call per repo.
	deletes := 0
	for _, call := range rec.ghCalls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "DELETE") && strings.Contains(joined, "labels/civm") {
			deletes++
		}
	}
	if deletes != 2 {
		t.Fatalf("gh DELETE label calls = %d, want 2 (calls=%v)", deletes, rec.ghCalls)
	}
	if !hasGHCall(rec.ghCalls, "api --method DELETE repos/acme/civm/actions/runners/11/labels/civm") {
		t.Fatalf("missing repository runner label DELETE: %v", rec.ghCalls)
	}
}

func TestEnterAndExitUseOrganizationRunnerLabelAPI(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = "  " + unitOrg + "  loaded active running GitHub Actions Runner\n"
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.GHFn = func(_ context.Context, args ...string) ([]byte, error) {
		rec.ghCalls = append(rec.ghCalls, append([]string{"gh"}, args...))
		if strings.Contains(strings.Join(args, " "), "orgs/acme/actions/runners?per_page=100") {
			return []byte(`[{"total_count":1,"runners":[{"id":71,"name":"civm-acme-org"}]}]`), nil
		}
		return nil, nil
	}

	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("Enter org runner err = %v", err)
	}
	rn, ok := runnerByName(state, "civm-acme-org")
	if !ok {
		t.Fatal("missing organization runner in state")
	}
	if !rn.Stopped || !rn.LabelRemoved || rn.RunnerID != 71 || rn.Repo != orgAcme {
		t.Fatalf("organization runner not fully drained: %+v", rn)
	}
	if !hasGHCall(rec.ghCalls, "api --method DELETE orgs/acme/actions/runners/71/labels/civm") {
		t.Fatalf("missing organization label DELETE: %v", rec.ghCalls)
	}

	rec.runCalls = nil
	rec.ghCalls = nil
	if _, err := Exit(context.Background(), opts); err != nil {
		t.Fatalf("Exit org runner err = %v", err)
	}
	if !hasGHCall(rec.ghCalls, "api --method POST orgs/acme/actions/runners/71/labels -f labels[]=civm") {
		t.Fatalf("missing organization label POST: %v", rec.ghCalls)
	}
}

func TestEnterReposFilterIncludesOrganizationRunnerForOwnedRepo(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = "  " + unitOrg + "  loaded active running GitHub Actions Runner\n"
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.Repos = []string{"acme/app", "emersonbusson/civm"}

	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("Enter filtered org runner err = %v", err)
	}
	if len(state.Runners) != 1 || state.Runners[0].Name != "civm-acme-org" {
		t.Fatalf("filtered organization runners = %+v", state.Runners)
	}
}

func TestResolveRunnerIDRejectsDuplicateName(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	opts := rec.options(filepath.Join(t.TempDir(), "maintenance.json"))
	opts.GHFn = func(context.Context, ...string) ([]byte, error) {
		return []byte(`[{"runners":[{"id":70,"name":"civm-acme-org"}]},{"runners":[{"id":71,"name":"civm-acme-org"}]}]`), nil
	}
	if _, err := resolveRunnerID(context.Background(), opts, "orgs/acme", "civm-acme-org"); err == nil {
		t.Fatal("resolveRunnerID must reject duplicate runner names")
	}
}

func hasGHCall(calls [][]string, want string) bool {
	for _, call := range calls {
		if strings.Join(call[1:], " ") == want {
			return true
		}
	}
	return false
}

func TestExitRestoresRecordedStateAndDeletesIt(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("Enter err = %v", err)
	}
	rec.runCalls = nil
	rec.ghCalls = nil

	state, err := Exit(context.Background(), rec.options(statePath))
	if err != nil {
		t.Fatalf("Exit err = %v", err)
	}
	if len(state.Runners) != 2 {
		t.Fatalf("Exit state runners = %d, want 2", len(state.Runners))
	}
	starts := 0
	for _, call := range rec.runCalls {
		if strings.Contains(strings.Join(call, " "), "systemctl start") {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("systemctl start calls = %d, want 2 (calls=%v)", starts, rec.runCalls)
	}
	posts := 0
	for _, call := range rec.ghCalls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "POST") && strings.Contains(joined, "labels[]=civm") {
			posts++
		}
	}
	if posts != 2 {
		t.Fatalf("gh POST label calls = %d, want 2 (calls=%v)", posts, rec.ghCalls)
	}
	if !hasGHCall(rec.ghCalls, "api --method POST repos/acme/civm/actions/runners/11/labels -f labels[]=civm") {
		t.Fatalf("missing repository runner label POST: %v", rec.ghCalls)
	}
	if len(rec.removed) != 1 || rec.removed[0] != statePath {
		t.Fatalf("removed = %v, want [%s]", rec.removed, statePath)
	}
}

func TestEnterReRunIsNoOpAndRefreshesDrainedAt(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("first Enter err = %v", err)
	}
	ghAfterFirst := len(rec.ghCalls)
	runAfterFirst := len(rec.runCalls)

	// Second Enter: same NowFn so DrainedAt is stable, but no new drain calls.
	state, err := Enter(context.Background(), rec.options(statePath))
	if err != nil {
		t.Fatalf("second Enter err = %v", err)
	}
	if state.DrainedAt != fixedTime {
		t.Fatalf("DrainedAt = %q, want refreshed %q", state.DrainedAt, fixedTime)
	}
	if len(rec.ghCalls) != ghAfterFirst {
		t.Fatalf("second Enter issued extra gh calls: %v", rec.ghCalls[ghAfterFirst:])
	}
	// Only the list-units probe is allowed; no stop/start mutations.
	stops := 0
	for _, call := range rec.runCalls[runAfterFirst:] {
		if strings.Contains(strings.Join(call, " "), "systemctl stop") {
			stops++
		}
	}
	if stops != 0 {
		t.Fatalf("second Enter issued %d stop calls, want 0", stops)
	}
}

func TestExitWithoutStateIsNoOp(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	state, err := Exit(context.Background(), rec.options(statePath))
	if err != nil {
		t.Fatalf("Exit err = %v", err)
	}
	if len(state.Runners) != 0 {
		t.Fatalf("expected empty state, got %+v", state)
	}
	if len(rec.runCalls) != 0 || len(rec.ghCalls) != 0 {
		t.Fatalf("no-op Exit mutated: run=%v gh=%v", rec.runCalls, rec.ghCalls)
	}
}

func TestDryRunEnterDoesNotMutate(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.Execute = false

	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("dry-run Enter err = %v", err)
	}
	if len(state.Runners) != 2 {
		t.Fatalf("dry-run preview runners = %d, want 2", len(state.Runners))
	}
	if _, ok := rec.files[statePath]; ok {
		t.Fatalf("dry-run must not write state file")
	}
	if rec.lockCalls != 0 {
		t.Fatalf("dry-run must not acquire lock, got %d", rec.lockCalls)
	}
	// No mutating run/gh calls (the list-units probe is allowed).
	for _, call := range rec.runCalls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "stop") || strings.Contains(joined, "start") {
			t.Fatalf("dry-run issued mutating run call: %v", call)
		}
	}
	if len(rec.ghCalls) != 0 {
		t.Fatalf("dry-run issued gh calls: %v", rec.ghCalls)
	}
}

func TestEnterBusyHostBlocksWithoutForce(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.idle = false
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	if _, err := Enter(context.Background(), rec.options(statePath)); err == nil {
		t.Fatalf("Enter on busy host must error")
	}
	if _, ok := rec.files[statePath]; ok {
		t.Fatalf("busy Enter must not write state")
	}
}

func TestEnterBusyHostProceedsWithForce(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.idle = false
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.Force = true

	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("forced Enter err = %v", err)
	}
	if len(state.Runners) != 2 {
		t.Fatalf("forced Enter runners = %d, want 2", len(state.Runners))
	}
}

func TestEnterPartialFailureStopOnlyRecordsCorrectly(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = "  " + unitCivm + "  loaded active running GitHub Actions Runner\n"
	// gh always fails -> label not removed; systemctl stop succeeds.
	rec.ghErrOn = func([]string) error { return errFake }
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	state, err := Enter(context.Background(), rec.options(statePath))
	if err != nil {
		t.Fatalf("Enter with label failure should not error (stop succeeded): %v", err)
	}
	rn, ok := runnerByName(state, "runner-1")
	if !ok {
		t.Fatalf("missing runner-1")
	}
	if !rn.Stopped || rn.LabelRemoved {
		t.Fatalf("partial drain mis-recorded: %+v", rn)
	}

	// Exit must restore only the stop (start), never re-add the label.
	rec.runCalls = nil
	rec.ghCalls = nil
	if _, err := Exit(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("Exit err = %v", err)
	}
	for _, call := range rec.ghCalls {
		t.Fatalf("Exit must not call gh for a label that was never removed: %v", call)
	}
	starts := 0
	for _, call := range rec.runCalls {
		if strings.Contains(strings.Join(call, " "), "systemctl start") {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("Exit start calls = %d, want 1", starts)
	}
}

func TestEnterStrictPersistsPartialDrainAndFails(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	rec.runErrOn = func(name string, args []string) error {
		if name == "sudo" && strings.Join(args, " ") == "systemctl stop "+unitVitae {
			return errFake
		}
		return nil
	}
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.RequireStopped = true

	state, err := Enter(context.Background(), opts)
	if err == nil {
		t.Fatal("strict enter must fail when one listener did not stop")
	}
	if _, ok := rec.files[statePath]; !ok {
		t.Fatal("strict partial drain must persist recoverable state")
	}
	first, ok := runnerByName(state, "runner-1")
	if !ok || !first.Stopped {
		t.Fatalf("runner-1 state = %+v, want stopped", first)
	}
	second, ok := runnerByName(state, "runner-2")
	if !ok || second.Stopped {
		t.Fatalf("runner-2 state = %+v, want not stopped", second)
	}
}

func TestEnterStrictRetriesOnlyPendingRunner(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	failSecond := true
	rec.runErrOn = func(name string, args []string) error {
		if failSecond && name == "sudo" && strings.Join(args, " ") == "systemctl stop "+unitVitae {
			return errFake
		}
		return nil
	}
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.RequireStopped = true
	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatal("first strict enter must leave a retryable partial state")
	}

	failSecond = false
	rec.runCalls = nil
	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("strict retry err = %v", err)
	}
	for _, name := range []string{"runner-1", "runner-2"} {
		rn, ok := runnerByName(state, name)
		if !ok || !rn.Stopped {
			t.Fatalf("%s after retry = %+v, want stopped", name, rn)
		}
	}
	if hasRunCall(rec.runCalls, "sudo systemctl stop "+unitCivm) {
		t.Fatalf("strict retry stopped an already drained runner: %v", rec.runCalls)
	}
	if !hasRunCall(rec.runCalls, "sudo systemctl stop "+unitVitae) {
		t.Fatalf("strict retry did not stop the pending runner: %v", rec.runCalls)
	}
}

func TestEnterStrictRetryBusyDoesNotMutate(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	rec.runErrOn = func(name string, args []string) error {
		if name == "sudo" && strings.Join(args, " ") == "systemctl stop "+unitVitae {
			return errFake
		}
		return nil
	}
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.RequireStopped = true
	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatal("first strict enter must leave a retryable partial state")
	}

	rec.idle = false
	rec.runCalls = nil
	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatal("strict retry while busy must fail")
	}
	for _, call := range rec.runCalls {
		if strings.Contains(strings.Join(call, " "), "systemctl stop") {
			t.Fatalf("busy strict retry mutated runner units: %v", rec.runCalls)
		}
	}
}

func TestEnterStrictFailsClosedWhenRunnerDiscoveryIsUnknown(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.RequireStopped = true
	baseRun := opts.RunFn
	opts.RunFn = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && strings.Contains(strings.Join(args, " "), "list-units") {
			return nil, errFake
		}
		return baseRun(ctx, name, args...)
	}

	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatal("strict enter must fail when it cannot enumerate eligible runners")
	}
	if _, ok := rec.files[statePath]; ok {
		t.Fatal("strict enter must not write a complete-looking state after unknown discovery")
	}
}

func TestEnterStrictRemovesEveryLabelBeforeStoppingAnyRunner(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.RequireStopped = true
	baseRun := opts.RunFn
	baseGH := opts.GHFn
	labelRemovals := 0
	opts.GHFn = func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "--method DELETE") && strings.Contains(strings.Join(args, " "), "labels/civm") {
			labelRemovals++
		}
		return baseGH(ctx, args...)
	}
	opts.RunFn = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "sudo" && strings.Contains(strings.Join(args, " "), "systemctl stop") && labelRemovals != 2 {
			return nil, errors.New("strict stop raced label withdrawal")
		}
		return baseRun(ctx, name, args...)
	}

	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("strict enter must withdraw every label before stopping units: %v", err)
	}
	for _, name := range []string{"runner-1", "runner-2"} {
		runner, ok := runnerByName(state, name)
		if !ok || !runner.Stopped || !runner.LabelRemoved {
			t.Fatalf("strict state for %s = %+v, want label removed then stopped", name, runner)
		}
	}
}

func TestEnterStrictDoesNotStopWhenAnyLabelCannotBeWithdrawn(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.RequireStopped = true
	baseGH := opts.GHFn
	opts.GHFn = func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "repos/other/peer/actions/runners/12/labels/civm") {
			return nil, errFake
		}
		return baseGH(ctx, args...)
	}

	state, err := Enter(context.Background(), opts)
	if err == nil {
		t.Fatal("strict enter must fail when it cannot withdraw every runner label")
	}
	for _, runner := range state.Runners {
		if runner.Stopped {
			t.Fatalf("strict enter stopped %s despite incomplete label withdrawal", runner.Name)
		}
	}
}

func TestExitRestoresStoppedSubsetAfterStrictPartialDrain(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	rec.runErrOn = func(name string, args []string) error {
		if name == "sudo" && strings.Join(args, " ") == "systemctl stop "+unitVitae {
			return errFake
		}
		return nil
	}
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.RequireStopped = true
	if _, err := Enter(context.Background(), opts); err == nil {
		t.Fatal("first strict enter must leave a partial state")
	}

	rec.runCalls = nil
	if _, err := Exit(context.Background(), opts); err != nil {
		t.Fatalf("exit partial strict state: %v", err)
	}
	if !hasRunCall(rec.runCalls, "sudo systemctl start "+unitCivm) {
		t.Fatalf("exit did not restore the stopped unit: %v", rec.runCalls)
	}
	if hasRunCall(rec.runCalls, "sudo systemctl start "+unitVitae) {
		t.Fatalf("exit started a unit that strict drain never stopped: %v", rec.runCalls)
	}
}

func TestEnterErrorsWhenBothFailForEveryRunner(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	rec.ghErrOn = func([]string) error { return errFake }
	rec.runErrOn = func(name string, args []string) error {
		if name == "sudo" && len(args) >= 2 && args[1] == "stop" {
			return errFake
		}
		return nil
	}
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	if _, err := Enter(context.Background(), rec.options(statePath)); err == nil {
		t.Fatalf("Enter must error when stop AND label fail for every runner")
	}
	if _, ok := rec.files[statePath]; ok {
		t.Fatalf("failed Enter must not write state")
	}
}

func TestExitDeleteFailureIsError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("Enter err = %v", err)
	}

	opts := rec.options(statePath)
	opts.RemoveFn = func(string) error { return errFake }
	if _, err := Exit(context.Background(), opts); err == nil {
		t.Fatalf("Exit must error when state delete fails")
	}
}

func TestEnterLockFailureIsError(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.lockErr = errFake
	statePath := filepath.Join(t.TempDir(), "maintenance.json")

	if _, err := Enter(context.Background(), rec.options(statePath)); err == nil {
		t.Fatalf("Enter must error when lock cannot be acquired")
	}
}

func TestEnterReposFilter(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	opts := rec.options(statePath)
	opts.Repos = []string{repoVitae}

	state, err := Enter(context.Background(), opts)
	if err != nil {
		t.Fatalf("Enter err = %v", err)
	}
	if len(state.Runners) != 1 {
		t.Fatalf("filtered Runners = %d, want 1", len(state.Runners))
	}
	if state.Runners[0].Repo != repoVitae {
		t.Fatalf("filtered repo = %q, want %q", state.Runners[0].Repo, repoVitae)
	}
}

func TestStatePersistedJSONRoundTrips(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.listOutput = twoRunnerListing()
	statePath := filepath.Join(t.TempDir(), "maintenance.json")
	if _, err := Enter(context.Background(), rec.options(statePath)); err != nil {
		t.Fatalf("Enter err = %v", err)
	}
	var parsed State
	if err := json.Unmarshal(rec.files[statePath], &parsed); err != nil {
		t.Fatalf("persisted state is not valid JSON: %v", err)
	}
	if parsed.DrainedAt != fixedTime || len(parsed.Runners) != 2 {
		t.Fatalf("round-trip mismatch: %+v", parsed)
	}
}

func TestParseRunnerUnit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		unit     string
		wantRepo string
		wantName string
	}{
		{unitCivm, repoCivm, "runner-1"},
		{unitVitae, repoVitae, "runner-2"},
		{"not-a-runner.service", "", ""},
		{"actions.runner.solo.service", "solo", ""},
	}
	for _, c := range cases {
		repo, name := parseRunnerUnit(c.unit)
		if repo != c.wantRepo || name != c.wantName {
			t.Fatalf("parseRunnerUnit(%q) = (%q,%q), want (%q,%q)",
				c.unit, repo, name, c.wantRepo, c.wantName)
		}
	}
}

func TestRunnerUnitNameRoundTrip(t *testing.T) {
	t.Parallel()
	rn := RunnerState{Name: "runner-1", Repo: repoCivm}
	if got := runnerUnitName(rn); got != unitCivm {
		t.Fatalf("runnerUnitName = %q, want %q", got, unitCivm)
	}
}

func TestFirstUnitFieldTolatesMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line string
		want string
	}{
		{"  " + unitCivm + " loaded active running", unitCivm},
		{"● " + unitVitae + " loaded failed failed", unitVitae},
		{"○ " + unitCivm + " loaded inactive dead", unitCivm},
		{"", ""},
		{"UNIT LOAD ACTIVE SUB DESCRIPTION", ""},
		{"docker.service loaded active running Docker", ""},
	}
	for _, c := range cases {
		if got := firstUnitField(c.line); got != c.want {
			t.Fatalf("firstUnitField(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestRenderTextAndJSON(t *testing.T) {
	t.Parallel()
	state := State{DrainedAt: fixedTime, Runners: []RunnerState{
		{Name: "runner-1", Repo: repoCivm, Stopped: true, LabelRemoved: true},
	}}
	var jsonBuf strings.Builder
	if err := RenderJSON(&jsonBuf, state); err != nil {
		t.Fatalf("RenderJSON err = %v", err)
	}
	if !strings.Contains(jsonBuf.String(), repoCivm) {
		t.Fatalf("RenderJSON missing repo: %s", jsonBuf.String())
	}
	var textBuf strings.Builder
	RenderText(&textBuf, "enter", state)
	if !strings.Contains(textBuf.String(), "runner-1") || !strings.Contains(textBuf.String(), "enter") {
		t.Fatalf("RenderText unexpected: %s", textBuf.String())
	}
}
