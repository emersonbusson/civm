package jitdispatcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDispatcherHappyPathAndCompletedReplay(t *testing.T) {
	dispatcher, github, runner, store, request := newDispatcherFixture(t)
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if result.Status != StatusCompleted || result.RunID != 42 || result.Replay {
		t.Fatalf("Dispatch() result = %+v", result)
	}
	if runner.calls != 1 || github.dispatchCalls != 1 || github.jitCalls != 1 {
		t.Fatalf("effects: runner=%d dispatch=%d jit=%d", runner.calls, github.dispatchCalls, github.jitCalls)
	}
	ledger, found, err := store.Load(result.RequestID)
	if err != nil || !found || ledger.Status != StatusCompleted || ledger.RunnerLabel == "" {
		t.Fatalf("ledger = %+v, found=%v, err=%v", ledger, found, err)
	}
	before := github.totalCalls()
	replay, err := dispatcher.Dispatch(context.Background(), request)
	if err != nil || !replay.Replay || replay.Status != StatusCompleted {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if github.totalCalls() != before || runner.calls != 1 {
		t.Fatal("completed replay repeated a remote or runner effect")
	}
}

func TestDispatcherDeletesExactRunnerAndConfirmsAbsence(t *testing.T) {
	dispatcher, github, _, _, request := newDispatcherFixture(t)
	identity := identityFromFakeRun(t, github.runs[0])
	github.runnerPresent = true
	github.remoteRunner = RemoteRunner{
		ID: 99, Name: identity.RunnerName, Status: "offline",
		Labels: []RunnerLabel{{Name: "self-hosted", Type: "read-only"}, {Name: identity.Label, Type: "custom"}},
	}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.deleteRunnerCalls != 1 || github.getRunnerCalls != 2 || github.runnerPresent {
		t.Fatalf("runner cleanup calls/presence = delete:%d get:%d present:%v", github.deleteRunnerCalls, github.getRunnerCalls, github.runnerPresent)
	}
}

func TestDispatcherStartupReconciliationHoldsGuardUntilAllProofs(t *testing.T) {
	for _, test := range []struct {
		name       string
		withOrphan bool
	}{
		{name: "reacquire missing holder"},
		{name: "continue existing holder", withOrphan: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatcher, github, runner, store, request := newDispatcherFixture(t)
			ledger := interruptedLedger(t, dispatcher, github, request)
			if err := store.Save(ledger); err != nil {
				t.Fatal(err)
			}
			gate := dispatcher.deps.Gate.(*fakeGate)
			if test.withOrphan {
				gate.orphan = LeaseMarker{
					Version: 2, LeaseID: ledger.LeaseID, AdmissionID: strings.Repeat("a", 64),
					HolderPID: 44, StartTicks: 101,
				}
				gate.hasOrphan = true
			}
			result, err := dispatcher.Dispatch(context.Background(), request)
			if !errors.Is(err, ErrReplay) || !result.Replay || result.Status != StatusAmbiguous {
				t.Fatalf("Dispatch() after reconciliation = %+v, %v", result, err)
			}
			loaded, found, loadErr := store.Load(ledger.RequestID)
			if loadErr != nil || !found || !loaded.CleanupComplete || !loaded.RunTerminal || !loaded.RunnerGone || !loaded.IsolationGone {
				t.Fatalf("reconciled ledger = %+v, found=%v, err=%v", loaded, found, loadErr)
			}
			if runner.recoverCalls != 1 || github.cancelCalls != 1 || github.getRunnerCalls != 1 {
				t.Fatalf("recovery effects = recover:%d cancel:%d get-runner:%d", runner.recoverCalls, github.cancelCalls, github.getRunnerCalls)
			}
			if test.withOrphan {
				if gate.acquireCalls != 0 || gate.releaseOrphanCalls != 1 {
					t.Fatalf("orphan gate calls = acquire:%d release-orphan:%d", gate.acquireCalls, gate.releaseOrphanCalls)
				}
			} else if gate.acquireCalls != 1 || len(gate.leases) != 1 || gate.leases[0].releaseCalls != 1 {
				t.Fatalf("reacquired gate calls = acquire:%d leases:%+v", gate.acquireCalls, gate.leases)
			}
		})
	}
}

func TestDispatcherPreflightFailuresCreateNoRemoteEffect(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeGitHub, *Request)
	}{
		{"fork", func(_ *fakeGitHub, request *Request) { request.Repository = "fork/site" }},
		{"workflow drift", func(github *fakeGitHub, _ *Request) { github.workflow = []byte("drift") }},
		{"candidate mismatch", func(github *fakeGitHub, _ *Request) { github.candidateSHAs = []string{strings.Repeat("d", 40)} }},
		{"label race", func(github *fakeGitHub, _ *Request) { github.labelExists = true }},
		{"default branch drift", func(github *fakeGitHub, _ *Request) { github.defaultBranch = "develop" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dispatcher, github, runner, _, request := newDispatcherFixture(t)
			test.mutate(github, &request)
			if _, err := dispatcher.Dispatch(context.Background(), request); err == nil {
				t.Fatal("Dispatch() accepted a preflight failure")
			}
			if github.dispatchCalls != 0 || github.jitCalls != 0 || runner.calls != 0 {
				t.Fatalf("unsafe effects: dispatch=%d jit=%d runner=%d", github.dispatchCalls, github.jitCalls, runner.calls)
			}
		})
	}
}

func TestDispatcherLocalPreflightFailsBeforeNetworkOrGuardAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fakeRunner, *fakeGate)
	}{
		{name: "isolation driver", mutate: func(runner *fakeRunner, _ *fakeGate) {
			runner.preflightErr = errors.New("driver digest drift")
		}},
		{name: "Guard", mutate: func(_ *fakeRunner, gate *fakeGate) {
			gate.preflightErr = errors.New("Guard unavailable")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatcher, github, runner, _, request := newDispatcherFixture(t)
			gate := dispatcher.deps.Gate.(*fakeGate)
			test.mutate(runner, gate)
			if _, err := dispatcher.Dispatch(context.Background(), request); err == nil {
				t.Fatal("Dispatch() accepted failed local preflight")
			}
			if github.totalCalls() != 0 || gate.acquireCalls != 0 || runner.calls != 0 {
				t.Fatalf("local preflight created effects: github=%d gate=%d runner=%d", github.totalCalls(), gate.acquireCalls, runner.calls)
			}
		})
	}
}

func TestDispatcherFailsClosedWhenGuardAdmissionIsRejected(t *testing.T) {
	for _, test := range []struct {
		name            string
		admissionErr    error
		status          Status
		cleanupComplete bool
	}{
		{name: "busy", admissionErr: ErrBusy, status: StatusFailed, cleanupComplete: true},
		{name: "ambiguous", admissionErr: ErrAmbiguous, status: StatusAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatcher, github, runner, store, request := newDispatcherFixture(t)
			gate := dispatcher.deps.Gate.(*fakeGate)
			gate.acquireErr = test.admissionErr
			result, err := dispatcher.Dispatch(context.Background(), request)
			if !errors.Is(err, test.admissionErr) || result.Status != test.status {
				t.Fatalf("Dispatch() = %+v, %v", result, err)
			}
			if gate.acquireCalls != 1 || len(gate.leases) != 0 || github.dispatchCalls != 0 || github.jitCalls != 0 || runner.calls != 0 {
				t.Fatalf("admission effects: gate=%d leases=%d dispatch=%d jit=%d runner=%d", gate.acquireCalls, len(gate.leases), github.dispatchCalls, github.jitCalls, runner.calls)
			}
			ledger, found, loadErr := store.Load(result.RequestID)
			if loadErr != nil || !found || ledger.CleanupComplete != test.cleanupComplete {
				t.Fatalf("admission ledger = %+v, found=%v, err=%v", ledger, found, loadErr)
			}
		})
	}
}

func TestDispatcherRefusesAmbiguousDispatchWithoutJIT(t *testing.T) {
	dispatcher, github, runner, store, request := newDispatcherFixture(t)
	github.dispatchErr = fmtError(ErrAmbiguous, "legacy 204")
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.dispatchCalls != 1 || github.jitCalls != 0 || runner.calls != 0 || github.cancelCalls != 0 {
		t.Fatalf("unexpected effects: %+v", github)
	}
	ledger, _, loadErr := store.Load(result.RequestID)
	if loadErr != nil || ledger.Status != StatusAmbiguous || ledger.RunID != 0 {
		t.Fatalf("ledger = %+v, %v", ledger, loadErr)
	}
}

func TestDispatcherClassifiesPostEffectStateFailureAmbiguous(t *testing.T) {
	dispatcher, github, runner, store, request := newDispatcherFixture(t)
	github.onDispatch = func() {
		if err := os.Chmod(store.ledgerDir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(store.ledgerDir, 0o700) })
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.dispatchCalls != 1 || github.cancelCalls != 1 || github.jitCalls != 0 || runner.calls != 0 {
		t.Fatalf("effects: dispatch=%d cancel=%d jit=%d runner=%d", github.dispatchCalls, github.cancelCalls, github.jitCalls, runner.calls)
	}
}

func TestDispatcherRefusesDuplicateJobAndCancelsKnownRun(t *testing.T) {
	dispatcher, github, runner, store, request := newDispatcherFixture(t)
	forceCancellation(github)
	github.jobs = append(github.jobs, github.jobs[0])
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.cancelCalls != 1 || github.jitCalls != 0 || runner.calls != 0 {
		t.Fatalf("effects: cancel=%d jit=%d runner=%d", github.cancelCalls, github.jitCalls, runner.calls)
	}
	ledger, _, _ := store.Load(result.RequestID)
	if ledger.Status != StatusAmbiguous || ledger.FailureCode != "job_bind" {
		t.Fatalf("ledger = %+v", ledger)
	}
}

func TestDispatcherRefusesJITPartialFailureAndRetainsGuard(t *testing.T) {
	dispatcher, github, runner, store, request := newDispatcherFixture(t)
	forceCancellation(github)
	github.jitErr = fmtError(ErrAmbiguous, "partial response")
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.cancelCalls != 1 || github.jitCalls != 1 || runner.calls != 0 {
		t.Fatalf("effects: cancel=%d jit=%d runner=%d", github.cancelCalls, github.jitCalls, runner.calls)
	}
	ledger, found, loadErr := store.Load(result.RequestID)
	if loadErr != nil || !found || ledger.RunnerGone || ledger.CleanupComplete {
		t.Fatalf("ambiguous JIT ledger = %+v, found=%v, err=%v", ledger, found, loadErr)
	}
	gate := dispatcher.deps.Gate.(*fakeGate)
	if len(gate.leases) != 1 || gate.leases[0].releaseCalls != 0 || gate.leases[0].abandonCalls != 1 {
		t.Fatalf("ambiguous JIT released Guard: leases=%+v", gate.leases)
	}
}

func TestDispatcherReleasesGuardAfterAuthoritativeJITRejection(t *testing.T) {
	dispatcher, github, runner, store, request := newDispatcherFixture(t)
	forceCancellation(github)
	github.jitErr = &HTTPError{Operation: "generate JIT config", Status: 422}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err == nil || errors.Is(err, ErrAmbiguous) || result.Status != StatusFailed {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.cancelCalls != 1 || github.jitCalls != 1 || github.findRunnerCalls != 0 || runner.calls != 0 {
		t.Fatalf("effects: cancel=%d jit=%d find-runner=%d runner=%d", github.cancelCalls, github.jitCalls, github.findRunnerCalls, runner.calls)
	}
	ledger, found, loadErr := store.Load(result.RequestID)
	if loadErr != nil || !found || !ledger.RunnerGone || !ledger.CleanupComplete {
		t.Fatalf("rejected JIT ledger = %+v, found=%v, err=%v", ledger, found, loadErr)
	}
	gate := dispatcher.deps.Gate.(*fakeGate)
	if len(gate.leases) != 1 || gate.leases[0].releaseCalls != 1 || gate.leases[0].abandonCalls != 0 {
		t.Fatalf("authoritative JIT rejection retained Guard: leases=%+v", gate.leases)
	}
}

func TestDispatcherRefusesRunnerLabelRaceAfterJobBinding(t *testing.T) {
	dispatcher, github, runner, _, request := newDispatcherFixture(t)
	forceCancellation(github)
	github.labelResults = []bool{false, true}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.labelCalls != 2 || github.cancelCalls != 1 || github.jitCalls != 0 || runner.calls != 0 {
		t.Fatalf("effects: label=%d cancel=%d jit=%d runner=%d", github.labelCalls, github.cancelCalls, github.jitCalls, runner.calls)
	}
}

func TestDispatcherRejectsRunDriftBeforeJIT(t *testing.T) {
	dispatcher, github, runner, _, request := newDispatcherFixture(t)
	github.runs[0].HeadSHA = strings.Repeat("f", 40)
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err == nil || result.Status != StatusFailed {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.cancelCalls != 0 || github.jitCalls != 0 || runner.calls != 0 {
		t.Fatal("run drift reached JIT, runner, or unsafe cancellation")
	}
}

func TestDispatcherRejectsRunIdentityDriftAfterJIT(t *testing.T) {
	dispatcher, github, runner, _, request := newDispatcherFixture(t)
	github.runs[1].Path = ""
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.jitCalls != 1 || runner.calls != 1 || github.cancelCalls != 0 {
		t.Fatalf("effects: jit=%d runner=%d cancel=%d", github.jitCalls, runner.calls, github.cancelCalls)
	}
}

func TestDispatcherModelsRunnerTheftAsAmbiguousAndCleansGeneratedRunner(t *testing.T) {
	dispatcher, github, runner, _, request := newDispatcherFixture(t)
	github.executedJob.RunnerID = 100
	github.executedJob.RunnerName = "stolen-by-another-runner"
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() runner theft = %+v, %v", result, err)
	}
	if github.jitCalls != 1 || runner.calls != 1 || github.cancelCalls != 0 || github.getRunnerCalls != 1 {
		t.Fatalf("runner theft cleanup = jit:%d run:%d cancel:%d get-runner:%d", github.jitCalls, runner.calls, github.cancelCalls, github.getRunnerCalls)
	}
}

func TestDispatcherRecoversIsolationWhenDestroyedReceiptHasRunnerError(t *testing.T) {
	dispatcher, _, runner, store, request := newDispatcherFixture(t)
	runner.err = errors.New("containment remained populated")
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "containment remained populated") {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if runner.recoverCalls != 1 {
		t.Fatalf("Recover() calls = %d", runner.recoverCalls)
	}
	ledger, found, loadErr := store.Load(result.RequestID)
	if loadErr != nil || !found || !ledger.IsolationGone || !ledger.CleanupComplete {
		t.Fatalf("cleanup ledger = %+v, found=%v, err=%v", ledger, found, loadErr)
	}
}

func TestDispatcherIndependentlyRejectsRunnerLifecycleProof(t *testing.T) {
	dispatcher, _, runner, store, request := newDispatcherFixture(t)
	runner.mutateOutcome = func(outcome *RunnerOutcome) {
		outcome.Destroyed.ResetVerified = false
	}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if runner.recoverCalls != 1 {
		t.Fatalf("Recover() calls = %d", runner.recoverCalls)
	}
	ledger, found, loadErr := store.Load(result.RequestID)
	if loadErr != nil || !found || !ledger.IsolationGone || !ledger.CleanupComplete {
		t.Fatalf("cleanup ledger = %+v, found=%v, err=%v", ledger, found, loadErr)
	}
}

func TestDispatcherRetainsGuardWhenRecoveryProofIsInvalid(t *testing.T) {
	dispatcher, _, runner, store, request := newDispatcherFixture(t)
	runner.err = errors.New("driver outcome uncertain")
	runner.mutateRecovery = func(receipt *IsolationReceipt) {
		receipt.ResetVerified = false
	}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	ledger, found, loadErr := store.Load(result.RequestID)
	if loadErr != nil || !found || ledger.IsolationGone || ledger.CleanupComplete {
		t.Fatalf("unresolved recovery ledger = %+v, found=%v, err=%v", ledger, found, loadErr)
	}
	gate := dispatcher.deps.Gate.(*fakeGate)
	if len(gate.leases) != 1 || gate.leases[0].releaseCalls != 0 || gate.leases[0].abandonCalls != 1 {
		t.Fatalf("invalid recovery proof released Guard: leases=%+v", gate.leases)
	}
}

func TestDispatcherFindsAndDeletesRunnerAfterAmbiguousJITResponse(t *testing.T) {
	dispatcher, github, runner, _, request := newDispatcherFixture(t)
	identity := identityFromFakeRun(t, github.runs[0])
	github.jitErr = fmtError(ErrAmbiguous, "truncated JIT response")
	github.runnerPresent = true
	github.remoteRunner = RemoteRunner{
		ID: 99, Name: identity.RunnerName, Status: "offline",
		Labels: []RunnerLabel{{Name: "self-hosted", Type: "read-only"}, {Name: identity.Label, Type: "custom"}},
	}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() ambiguous JIT = %+v, %v", result, err)
	}
	if runner.calls != 0 || github.findRunnerCalls != 1 || github.deleteRunnerCalls != 1 || github.runnerPresent {
		t.Fatalf("ambiguous JIT recovery = run:%d find:%d delete:%d present:%v", runner.calls, github.findRunnerCalls, github.deleteRunnerCalls, github.runnerPresent)
	}
}

func TestDispatcherRejectsJobWithoutIdentity(t *testing.T) {
	dispatcher, github, runner, _, request := newDispatcherFixture(t)
	forceCancellation(github)
	github.jobs[0].ID = 0
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err == nil || result.Status != StatusFailed {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.jitCalls != 0 || runner.calls != 0 || github.cancelCalls != 1 {
		t.Fatalf("effects: jit=%d runner=%d cancel=%d", github.jitCalls, runner.calls, github.cancelCalls)
	}
}

func TestDispatcherTreatsCandidateRevalidationFailureAsAmbiguous(t *testing.T) {
	dispatcher, github, _, _, request := newDispatcherFixture(t)
	github.candidateRevalidateErr = errors.New("ref API unavailable")
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != StatusAmbiguous {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
}

func TestDispatcherMarksCandidateRefMovementStale(t *testing.T) {
	dispatcher, github, _, store, request := newDispatcherFixture(t)
	github.candidateSHAs = []string{request.CandidateSHA, strings.Repeat("e", 40)}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrStale) || result.Status != StatusStale {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	ledger, _, loadErr := store.Load(result.RequestID)
	if loadErr != nil || ledger.Status != StatusStale {
		t.Fatalf("ledger = %+v, %v", ledger, loadErr)
	}
}

func TestDispatcherPollsDirectRunAndJobIdentityWithoutDiscovery(t *testing.T) {
	dispatcher, github, _, _, request := newDispatcherFixture(t)
	github.runErrors = []error{&HTTPError{Operation: "get run", Status: 404}}
	github.jobResults = [][]WorkflowJob{nil, github.jobs}
	github.runs = []WorkflowRun{github.runs[0], {
		ID: 42, Event: "workflow_dispatch", HeadSHA: strings.Repeat("b", 40),
		DisplayTitle: github.runs[0].DisplayTitle, Path: github.runs[0].Path,
		Status: "in_progress",
	}, github.runs[1]}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.runCalls < 4 || github.jobCalls != 2 {
		t.Fatalf("poll calls: run=%d jobs=%d", github.runCalls, github.jobCalls)
	}
}

func TestDispatcherAcceptsCancel409OnlyForTerminalKnownRun(t *testing.T) {
	dispatcher, github, _, _, request := newDispatcherFixture(t)
	forceCancellation(github)
	github.jobs[0].Labels = []string{"wrong-label"}
	github.cancelErr = &HTTPError{Operation: "cancel", Status: 409}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if err == nil || result.Status != StatusFailed {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.cancelCalls != 1 || github.runCalls != 3 {
		t.Fatalf("cancel race calls: cancel=%d run=%d", github.cancelCalls, github.runCalls)
	}
}

func TestNewDispatcherDefaultsAndRejectsMissingDependencies(t *testing.T) {
	if _, err := NewDispatcher(Config{}, Dependencies{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewDispatcher() missing dependency error = %v", err)
	}
	_, github, runner, store, _ := newDispatcherFixture(t)
	dispatcher, err := NewDispatcher(Config{}, Dependencies{GitHub: github, Runner: runner, Store: store, Gate: &fakeGate{}})
	if err != nil || dispatcher.deps.Random == nil || dispatcher.deps.Sleep == nil || dispatcher.deps.Logger == nil {
		t.Fatalf("NewDispatcher() defaults = %+v, %v", dispatcher, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext() error = %v", err)
	}
}

func TestDispatcherFailsClosedOnUnrecoverableDispatchBeforeNetwork(t *testing.T) {
	dispatcher, github, runner, store, request := newDispatcherFixture(t)
	requestID := RequestID(request)
	now := time.Unix(1_700_000_000, 0).UTC()
	identity, err := NewIdentity(bytes.NewReader(bytes.Repeat([]byte{'z'}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Ledger{
		Version: ledgerVersion, RequestID: requestID, Repository: request.Repository,
		CandidateRef: request.CandidateRef, CandidateSHA: request.CandidateSHA,
		TrustedSHA: github.trustedSHA, Workflow: ".github/workflows/civm-jit.yml",
		WorkflowSHA256: strings.Repeat("a", 64), Nonce: identity.Nonce,
		RunnerLabel: identity.Label, RunnerName: identity.RunnerName, WorkFolder: identity.WorkFolder,
		LeaseID: identity.Nonce, IsolationGone: true, RunnerGone: true,
		Status: StatusDispatching, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Dispatch(context.Background(), request)
	if !errors.Is(err, ErrAmbiguous) || result.Status != "" {
		t.Fatalf("Dispatch() = %+v, %v", result, err)
	}
	if github.totalCalls() != 0 || runner.calls != 0 {
		t.Fatal("nonterminal replay reached a remote effect")
	}
	gate := dispatcher.deps.Gate.(*fakeGate)
	if gate.acquireCalls != 1 || len(gate.leases) != 1 || gate.leases[0].releaseCalls != 0 || gate.leases[0].abandonCalls != 1 {
		t.Fatalf("unresolved recovery released Guard: acquire=%d leases=%+v", gate.acquireCalls, gate.leases)
	}
}

func forceCancellation(github *fakeGitHub) {
	queued := github.runs[0]
	completed := github.runs[len(github.runs)-1]
	github.runs = []WorkflowRun{queued, queued, completed}
}

func interruptedLedger(t *testing.T, dispatcher *Dispatcher, github *fakeGitHub, request Request) Ledger {
	t.Helper()
	identity := identityFromFakeRun(t, github.runs[0])
	policy, ok := dispatcher.config.Policy(request.Repository)
	if !ok {
		t.Fatal("fixture policy unavailable")
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	return Ledger{
		Version: ledgerVersion, RequestID: RequestID(request), Repository: request.Repository,
		CandidateRef: request.CandidateRef, CandidateSHA: request.CandidateSHA,
		TrustedSHA: github.trustedSHA, Workflow: policy.Workflow, WorkflowSHA256: policy.WorkflowSHA256,
		Nonce: identity.Nonce, RunnerLabel: identity.Label, RunnerName: identity.RunnerName,
		WorkFolder: identity.WorkFolder, RunID: 42, JobID: 7, RunnerID: 99,
		LeaseID: identity.Nonce, IsolationID: "disposable-vm-0009",
		IsolationBase: dispatcher.config.BaseImageSHA256,
		Status:        StatusIsolationReady, CreatedAt: now, UpdatedAt: now,
	}
}

func identityFromFakeRun(t *testing.T, run WorkflowRun) Identity {
	t.Helper()
	nonce := strings.TrimPrefix(run.DisplayTitle, "CIVM JIT ")
	if len(nonce) < 16 {
		t.Fatalf("fake run has no nonce: %+v", run)
	}
	identity := Identity{
		Nonce: nonce, Label: "civm-jit-" + nonce,
		RunnerName: "civm-jit-" + nonce[:16], WorkFolder: "_work/jit-" + nonce,
	}
	if !validIdentity(identity) {
		t.Fatalf("fake run has invalid identity: %+v", run)
	}
	return identity
}

func newDispatcherFixture(t *testing.T) (*Dispatcher, *fakeGitHub, *fakeRunner, *Store, Request) {
	t.Helper()
	workflow := []byte("name: trusted workflow\n")
	digest := sha256.Sum256(workflow)
	request := Request{
		Repository: "acme/site", CandidateRef: "refs/heads/feature/safe",
		CandidateSHA: strings.Repeat("a", 40), Idempotency: "request-00000001",
	}
	policy := RepositoryPolicy{
		Repository: request.Repository, TrustedRef: "refs/heads/main",
		Workflow: ".github/workflows/civm-jit.yml", WorkflowSHA256: hex.EncodeToString(digest[:]),
		CandidateRefs: []string{request.CandidateRef}, RunnerGroupID: 1, JobName: "trusted-jit",
	}
	config := Config{
		APIBaseURL: "https://api.github.com", APIVersion: SupportedAPIVersion,
		StateDir: filepath.Join(t.TempDir(), "state"), RunnerDirectory: filepath.Join(t.TempDir(), "runner"),
		GuardExecutable: "/usr/local/bin/guard", IsolationDriver: "/usr/local/libexec/civm-jit-isolation",
		DriverSHA256: strings.Repeat("c", 64), BaseImageSHA256: strings.Repeat("d", 64),
		QueueWait: time.Second, QueuePoll: time.Millisecond, HTTPTimeout: time.Second,
		JobPollInterval: time.Millisecond, JobBindTimeout: 20 * time.Millisecond,
		RunTimeout: time.Second, ShutdownGrace: 10 * time.Millisecond, RecoveryTimeout: time.Second,
		Repositories: []RepositoryPolicy{policy},
	}
	store, err := OpenStore(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := NewIdentity(bytes.NewReader(bytes.Repeat([]byte{'x'}, 32)))
	github := &fakeGitHub{
		defaultBranch: "main", trustedSHA: strings.Repeat("b", 40),
		candidateSHAs: []string{request.CandidateSHA, request.CandidateSHA}, workflow: workflow,
		receipt: DispatchReceipt{RunID: 42, RunURL: "https://api.github.test/run/42", HTMLURL: "https://github.test/run/42"},
		runs: []WorkflowRun{
			{ID: 42, Event: "workflow_dispatch", HeadSHA: strings.Repeat("b", 40), DisplayTitle: "CIVM JIT " + identity.Nonce, Path: policy.Workflow + "@main", Status: "queued"},
			{ID: 42, Event: "workflow_dispatch", HeadSHA: strings.Repeat("b", 40), DisplayTitle: "CIVM JIT " + identity.Nonce, Path: policy.Workflow + "@main", Status: "completed", Conclusion: "success"},
		},
		jobs: []WorkflowJob{{ID: 7, RunID: 42, Name: policy.JobName, Status: "queued", Labels: []string{identity.Label}}},
		executedJob: WorkflowJob{ID: 7, RunID: 42, Name: policy.JobName, Status: "completed", Conclusion: "success",
			Labels: []string{identity.Label}, RunnerID: 99, RunnerName: identity.RunnerName, RunnerGroupID: 1},
		jit: []byte("encoded-jit-secret-value"),
	}
	runner := &fakeRunner{}
	dispatcher, err := NewDispatcher(config, Dependencies{
		GitHub: github, Store: store, Runner: runner, Gate: &fakeGate{},
		Random: bytes.NewReader(bytes.Repeat([]byte{'x'}, 32)),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Sleep:  sleepContext, Sensitive: [][]byte{[]byte(testToken)},
		AcquireQueue: func(context.Context, string, time.Duration) (io.Closer, error) {
			return io.NopCloser(strings.NewReader("")), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, github, runner, store, request
}

type fakeGitHub struct {
	defaultBranch          string
	trustedSHA             string
	candidateSHAs          []string
	candidateRead          int
	candidateRevalidateErr error
	workflow               []byte
	labelExists            bool
	labelResults           []bool
	labelRead              int
	receipt                DispatchReceipt
	dispatchErr            error
	onDispatch             func()
	jit                    []byte
	jitErr                 error
	runs                   []WorkflowRun
	runErrors              []error
	runRead                int
	jobs                   []WorkflowJob
	executedJob            WorkflowJob
	jobResults             [][]WorkflowJob
	jobRead                int
	cancelErr              error

	defaultCalls      int
	resolveCalls      int
	workflowCalls     int
	labelCalls        int
	dispatchCalls     int
	runCalls          int
	jobCalls          int
	jitCalls          int
	cancelCalls       int
	getJobCalls       int
	getRunnerCalls    int
	deleteRunnerCalls int
	findRunnerCalls   int
	runnerPresent     bool
	remoteRunner      RemoteRunner
}

func (fake *fakeGitHub) DefaultBranch(context.Context, string) (string, error) {
	fake.defaultCalls++
	return fake.defaultBranch, nil
}

func (fake *fakeGitHub) ResolveRef(_ context.Context, _ string, ref string) (string, error) {
	fake.resolveCalls++
	if ref == "refs/heads/main" {
		return fake.trustedSHA, nil
	}
	if len(fake.candidateSHAs) == 0 {
		return "", errors.New("candidate SHA unavailable")
	}
	if fake.candidateRead > 0 && fake.candidateRevalidateErr != nil {
		fake.candidateRead++
		return "", fake.candidateRevalidateErr
	}
	index := min(fake.candidateRead, len(fake.candidateSHAs)-1)
	fake.candidateRead++
	return fake.candidateSHAs[index], nil
}

func (fake *fakeGitHub) WorkflowContent(context.Context, string, string, string) ([]byte, error) {
	fake.workflowCalls++
	return append([]byte(nil), fake.workflow...), nil
}

func (fake *fakeGitHub) RunnerLabelExists(context.Context, string, string) (bool, error) {
	fake.labelCalls++
	if len(fake.labelResults) > 0 {
		index := min(fake.labelRead, len(fake.labelResults)-1)
		fake.labelRead++
		return fake.labelResults[index], nil
	}
	return fake.labelExists, nil
}

func (fake *fakeGitHub) DispatchWorkflow(context.Context, string, string, string, map[string]string) (DispatchReceipt, error) {
	fake.dispatchCalls++
	if fake.onDispatch != nil {
		fake.onDispatch()
	}
	return fake.receipt, fake.dispatchErr
}

func (fake *fakeGitHub) GetRun(context.Context, string, int64) (WorkflowRun, error) {
	fake.runCalls++
	if len(fake.runErrors) > 0 {
		err := fake.runErrors[0]
		fake.runErrors = fake.runErrors[1:]
		if err != nil {
			return WorkflowRun{}, err
		}
	}
	if len(fake.runs) == 0 {
		return WorkflowRun{}, errors.New("no fake run")
	}
	index := min(fake.runRead, len(fake.runs)-1)
	fake.runRead++
	return fake.runs[index], nil
}

func (fake *fakeGitHub) ListJobs(context.Context, string, int64) ([]WorkflowJob, error) {
	fake.jobCalls++
	if len(fake.jobResults) > 0 {
		index := min(fake.jobRead, len(fake.jobResults)-1)
		fake.jobRead++
		return append([]WorkflowJob(nil), fake.jobResults[index]...), nil
	}
	return append([]WorkflowJob(nil), fake.jobs...), nil
}

func (fake *fakeGitHub) GetJob(context.Context, string, int64) (WorkflowJob, error) {
	fake.getJobCalls++
	return fake.executedJob, nil
}

func (fake *fakeGitHub) GenerateJIT(_ context.Context, _ string, identity Identity, _ int64) (JITRegistration, error) {
	fake.jitCalls++
	if fake.jitErr != nil {
		return JITRegistration{}, fake.jitErr
	}
	return JITRegistration{
		Secret: &JITSecret{value: append([]byte(nil), fake.jit...)},
		Runner: RemoteRunner{ID: 99, Name: identity.RunnerName, Status: "offline",
			Labels: []RunnerLabel{{Name: "self-hosted", Type: "read-only"}, {Name: identity.Label, Type: "custom"}}},
	}, nil
}

func (fake *fakeGitHub) FindRunnerByLabel(_ context.Context, _ string, label string) (RemoteRunner, bool, error) {
	fake.findRunnerCalls++
	if !fake.runnerPresent || !runnerHasLabel(fake.remoteRunner, label) {
		return RemoteRunner{}, false, nil
	}
	return fake.remoteRunner, true, nil
}

func (fake *fakeGitHub) GetRunner(context.Context, string, int64) (RemoteRunner, bool, error) {
	fake.getRunnerCalls++
	return fake.remoteRunner, fake.runnerPresent, nil
}

func (fake *fakeGitHub) DeleteRunner(context.Context, string, int64) error {
	fake.deleteRunnerCalls++
	fake.runnerPresent = false
	return nil
}

func (fake *fakeGitHub) CancelRun(context.Context, string, int64) error {
	fake.cancelCalls++
	return fake.cancelErr
}

func (fake *fakeGitHub) totalCalls() int {
	return fake.defaultCalls + fake.resolveCalls + fake.workflowCalls + fake.labelCalls +
		fake.dispatchCalls + fake.runCalls + fake.jobCalls + fake.jitCalls + fake.cancelCalls
}

type fakeRunner struct {
	calls          int
	recoverCalls   int
	err            error
	preflightErr   error
	mutateOutcome  func(*RunnerOutcome)
	mutateRecovery func(*IsolationReceipt)
}

func (fake *fakeRunner) Preflight(Config) error { return fake.preflightErr }

func (fake *fakeRunner) Run(_ context.Context, request RunnerRequest) (RunnerOutcome, error) {
	fake.calls++
	process := ProcessIdentity{
		PID: 123, StartTicks: 456, ProcessGroup: 123,
		CgroupPath: "/sys/fs/cgroup/civm-jit-" + request.LeaseID,
	}
	if request.OnStarted != nil {
		if err := request.OnStarted(process); err != nil {
			return RunnerOutcome{}, err
		}
	}
	ready := IsolationReceipt{Protocol: 1, Event: "ready", LeaseID: request.LeaseID,
		IsolationID: "isolation-00000001", BaseSHA256: request.BaseImageSHA256, Disposable: true}
	if request.OnReady != nil {
		if err := request.OnReady(ready); err != nil {
			return RunnerOutcome{}, err
		}
	}
	destroyed := ready
	destroyed.Event = "destroyed"
	destroyed.Destroyed = true
	destroyed.ResetVerified = true
	outcome := RunnerOutcome{Process: process, Ready: ready, Destroyed: destroyed}
	if fake.mutateOutcome != nil {
		fake.mutateOutcome(&outcome)
	}
	return outcome, fake.err
}

func (fake *fakeRunner) Recover(_ context.Context, request RecoveryRequest) (IsolationReceipt, error) {
	fake.recoverCalls++
	isolationID := request.IsolationID
	if isolationID == "" {
		isolationID = "recovered-isolation-0001"
	}
	receipt := IsolationReceipt{Protocol: 1, Event: "destroyed", LeaseID: request.LeaseID,
		IsolationID: isolationID, BaseSHA256: request.BaseImageSHA256,
		Disposable: true, Destroyed: true, ResetVerified: true}
	if fake.mutateRecovery != nil {
		fake.mutateRecovery(&receipt)
	}
	return receipt, nil
}

type fakeGate struct {
	orphan             LeaseMarker
	hasOrphan          bool
	acquireCalls       int
	releaseOrphanCalls int
	leases             []*fakeLease
	preflightErr       error
	acquireErr         error
}

func (gate *fakeGate) Preflight(string) error { return gate.preflightErr }

func (gate *fakeGate) Acquire(_ context.Context, _ string, leaseID string, _, _ time.Duration) (ResourceLease, error) {
	gate.acquireCalls++
	if gate.acquireErr != nil {
		return nil, gate.acquireErr
	}
	lease := &fakeLease{marker: LeaseMarker{
		Version: 2, LeaseID: leaseID, AdmissionID: strings.Repeat("f", 64),
		HolderPID: 42, StartTicks: 99,
	}}
	gate.leases = append(gate.leases, lease)
	return lease, nil
}

func (gate *fakeGate) Orphan() (LeaseMarker, bool, error) { return gate.orphan, gate.hasOrphan, nil }
func (gate *fakeGate) ReleaseOrphan(_ context.Context, marker LeaseMarker, _ time.Duration) error {
	gate.releaseOrphanCalls++
	if !gate.hasOrphan || marker != gate.orphan {
		return errors.New("unexpected orphan release")
	}
	gate.hasOrphan = false
	return nil
}

type fakeLease struct {
	marker       LeaseMarker
	releaseCalls int
	abandonCalls int
}

func (lease *fakeLease) Marker() LeaseMarker { return lease.marker }
func (lease *fakeLease) Release(context.Context) error {
	lease.releaseCalls++
	return nil
}
func (lease *fakeLease) Abandon() error {
	lease.abandonCalls++
	return nil
}

func fmtError(base error, detail string) error {
	return errors.Join(base, errors.New(detail))
}
