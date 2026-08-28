package jitdispatcher

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

type QueueAcquire func(context.Context, string, time.Duration) (io.Closer, error)

type Dependencies struct {
	GitHub       GitHub
	Store        *Store
	Runner       Runner
	Gate         ResourceGate
	Random       io.Reader
	Logger       *slog.Logger
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	AcquireQueue QueueAcquire
	Sensitive    [][]byte
}

type Dispatcher struct {
	config Config
	deps   Dependencies
}

func NewDispatcher(config Config, dependencies Dependencies) (*Dispatcher, error) {
	if dependencies.GitHub == nil || dependencies.Store == nil || dependencies.Runner == nil || dependencies.Gate == nil {
		return nil, fmt.Errorf("%w: dispatcher dependencies are incomplete", ErrInvalid)
	}
	if dependencies.Random == nil {
		dependencies.Random = rand.Reader
	}
	if dependencies.Logger == nil {
		dependencies.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if dependencies.Sleep == nil {
		dependencies.Sleep = sleepContext
	}
	if dependencies.AcquireQueue == nil {
		dependencies.AcquireQueue = AcquireQueue
	}
	return &Dispatcher{config: config, deps: dependencies}, nil
}

func (dispatcher *Dispatcher) Dispatch(ctx context.Context, request Request) (Result, error) {
	policy, ok := dispatcher.config.Policy(request.Repository)
	if !ok {
		return Result{}, fmt.Errorf("%w: repository is not allowlisted", ErrInvalid)
	}
	if err := ValidateRequest(request, policy); err != nil {
		return Result{}, err
	}
	if err := dispatcher.deps.Runner.Preflight(dispatcher.config); err != nil {
		return Result{}, fmt.Errorf("local isolation preflight failed: %w", err)
	}
	if err := dispatcher.deps.Gate.Preflight(dispatcher.config.GuardExecutable); err != nil {
		return Result{}, fmt.Errorf("local Guard preflight failed: %w", err)
	}
	queueCtx, cancelQueue := context.WithTimeout(ctx, dispatcher.config.QueueWait)
	defer cancelQueue()
	lock, err := dispatcher.deps.AcquireQueue(queueCtx, MachineLockPath, dispatcher.config.QueuePoll)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err := dispatcher.reconcileStartup(ctx); err != nil {
		return Result{}, fmt.Errorf("startup reconciliation failed closed: %w", err)
	}
	requestID := RequestID(request)
	if replay, found, err := dispatcher.replay(requestID, request); found || err != nil {
		return replay, err
	}
	trustedSHA, err := dispatcher.verifyAuthority(ctx, request, policy)
	if err != nil {
		return Result{}, err
	}
	identity, err := NewIdentity(dispatcher.deps.Random)
	if err != nil {
		return Result{}, err
	}
	collision, err := dispatcher.deps.GitHub.RunnerLabelExists(ctx, request.Repository, identity.Label)
	if err != nil {
		return Result{}, fmt.Errorf("check runner label: %w", err)
	}
	if collision {
		return Result{}, fmt.Errorf("runner label collision")
	}
	ledger := dispatcher.newLedger(requestID, request, policy, trustedSHA, identity)
	if err := dispatcher.deps.Store.Save(ledger); err != nil {
		return Result{}, err
	}
	lease, err := dispatcher.deps.Gate.Acquire(
		ctx, dispatcher.config.GuardExecutable, ledger.LeaseID,
		dispatcher.config.QueuePoll, dispatcher.config.ShutdownGrace,
	)
	if err != nil {
		ledger.RunTerminal = true
		ledger.RunnerGone = true
		ledger.IsolationGone = true
		ledger.CleanupComplete = !errors.Is(err, ErrAmbiguous)
		return dispatcher.finishWithoutLease(&ledger, "guard_admission", err)
	}
	released := false
	defer func() {
		if !released {
			_ = lease.Abandon()
		}
	}()
	result, dispatchErr := dispatcher.dispatchPrepared(ctx, request, policy, &ledger, identity)
	if ledger.CleanupComplete {
		releaseCtx, cancel := context.WithTimeout(context.Background(), dispatcher.config.ShutdownGrace)
		releaseErr := lease.Release(releaseCtx)
		cancel()
		released = true
		if releaseErr != nil {
			ledger.CleanupComplete = false
			_ = dispatcher.transition(&ledger, StatusAmbiguous, "guard_release")
			return dispatcher.result(&ledger), errors.Join(dispatchErr, fmt.Errorf("%w: Guard lease release failed: %v", ErrAmbiguous, releaseErr))
		}
	}
	return result, dispatchErr
}

func (dispatcher *Dispatcher) replay(requestID string, request Request) (Result, bool, error) {
	ledger, found, err := dispatcher.deps.Store.Load(requestID)
	if err != nil || !found {
		return Result{}, false, err
	}
	if ledger.Repository != request.Repository || ledger.CandidateRef != request.CandidateRef || ledger.CandidateSHA != request.CandidateSHA {
		return Result{}, true, fmt.Errorf("%w: ledger request mismatch", ErrReplay)
	}
	result := Result{RequestID: requestID, RunID: ledger.RunID, Status: ledger.Status, Replay: true}
	if ledger.Status == StatusCompleted && ledger.CleanupComplete {
		return result, true, nil
	}
	return result, true, fmt.Errorf("%w: existing request is %s", ErrReplay, ledger.Status)
}

func (dispatcher *Dispatcher) reconcileStartup(ctx context.Context) error {
	ledgers, err := dispatcher.deps.Store.List()
	if err != nil {
		return err
	}
	orphan, hasOrphan, err := dispatcher.deps.Gate.Orphan()
	if err != nil {
		return err
	}
	matchedOrphan := false
	incomplete := -1
	for index := range ledgers {
		if hasOrphan && ledgers[index].LeaseID == orphan.LeaseID {
			matchedOrphan = true
		}
		if ledgers[index].CleanupComplete {
			continue
		}
		if incomplete >= 0 {
			return fmt.Errorf("multiple incomplete ledgers violate global admission")
		}
		incomplete = index
	}
	if hasOrphan && !matchedOrphan {
		return fmt.Errorf("Guard lease has no matching durable ledger")
	}
	if incomplete < 0 {
		if !hasOrphan {
			return nil
		}
		return dispatcher.releaseOrphan(ctx, orphan)
	}
	ledger := &ledgers[incomplete]
	if hasOrphan && ledger.LeaseID != orphan.LeaseID {
		return fmt.Errorf("incomplete ledger does not own the active Guard lease")
	}

	var recoveryLease ResourceLease
	recoveryLeaseReleased := false
	if !hasOrphan {
		recoveryLease, err = dispatcher.deps.Gate.Acquire(
			ctx, dispatcher.config.GuardExecutable, ledger.LeaseID,
			dispatcher.config.QueuePoll, dispatcher.config.ShutdownGrace,
		)
		if err != nil {
			return fmt.Errorf("reacquire Guard for recovery: %w", err)
		}
		defer func() {
			if !recoveryLeaseReleased {
				_ = recoveryLease.Abandon()
			}
		}()
	}
	if err := dispatcher.transition(ledger, StatusReconciling, "startup_reconciliation"); err != nil {
		return err
	}
	recoveryCtx, cancel := context.WithTimeout(ctx, dispatcher.config.RecoveryTimeout)
	err = dispatcher.cleanupLedger(recoveryCtx, ledger)
	cancel()
	if err != nil {
		return err
	}
	ledger.Status = StatusAmbiguous
	ledger.FailureCode = "reconciled_after_restart"
	ledger.UpdatedAt = dispatcher.deps.Now().UTC()
	if err := dispatcher.deps.Store.Save(*ledger); err != nil {
		return err
	}
	if hasOrphan {
		return dispatcher.releaseOrphan(ctx, orphan)
	}
	releaseCtx, releaseCancel := context.WithTimeout(ctx, dispatcher.config.ShutdownGrace)
	err = recoveryLease.Release(releaseCtx)
	releaseCancel()
	recoveryLeaseReleased = true
	return err
}

func (dispatcher *Dispatcher) releaseOrphan(ctx context.Context, orphan LeaseMarker) error {
	releaseCtx, cancel := context.WithTimeout(ctx, dispatcher.config.ShutdownGrace)
	defer cancel()
	return dispatcher.deps.Gate.ReleaseOrphan(releaseCtx, orphan, dispatcher.config.ShutdownGrace)
}

func (dispatcher *Dispatcher) verifyAuthority(
	ctx context.Context,
	request Request,
	policy RepositoryPolicy,
) (string, error) {
	defaultBranch, err := dispatcher.deps.GitHub.DefaultBranch(ctx, request.Repository)
	if err != nil {
		return "", fmt.Errorf("get default branch: %w", err)
	}
	if policy.TrustedRef != "refs/heads/"+defaultBranch {
		return "", fmt.Errorf("trusted ref is not the default branch")
	}
	trustedSHA, err := dispatcher.deps.GitHub.ResolveRef(ctx, request.Repository, policy.TrustedRef)
	if err != nil {
		return "", fmt.Errorf("resolve trusted ref: %w", err)
	}
	workflow, err := dispatcher.deps.GitHub.WorkflowContent(ctx, request.Repository, policy.Workflow, trustedSHA)
	if err != nil {
		return "", fmt.Errorf("read trusted workflow: %w", err)
	}
	digest := sha256.Sum256(workflow)
	if hex.EncodeToString(digest[:]) != policy.WorkflowSHA256 {
		return "", fmt.Errorf("trusted workflow digest drift")
	}
	candidateSHA, err := dispatcher.deps.GitHub.ResolveRef(ctx, request.Repository, request.CandidateRef)
	if err != nil {
		return "", fmt.Errorf("resolve candidate ref: %w", err)
	}
	if candidateSHA != request.CandidateSHA {
		return "", fmt.Errorf("candidate SHA does not match its allowlisted ref")
	}
	return trustedSHA, nil
}

func (dispatcher *Dispatcher) newLedger(
	requestID string,
	request Request,
	policy RepositoryPolicy,
	trustedSHA string,
	identity Identity,
) Ledger {
	now := dispatcher.deps.Now().UTC()
	return Ledger{
		Version: ledgerVersion, RequestID: requestID, Repository: request.Repository,
		CandidateRef: request.CandidateRef, CandidateSHA: request.CandidateSHA,
		TrustedSHA: trustedSHA, Workflow: policy.Workflow, WorkflowSHA256: policy.WorkflowSHA256,
		Nonce: identity.Nonce, RunnerLabel: identity.Label, RunnerName: identity.RunnerName,
		WorkFolder: identity.WorkFolder, LeaseID: identity.Nonce,
		RunTerminal: true, RunnerGone: true, IsolationGone: true,
		Status: StatusPrepared, CreatedAt: now, UpdatedAt: now,
	}
}

func (dispatcher *Dispatcher) dispatchPrepared(
	ctx context.Context,
	request Request,
	policy RepositoryPolicy,
	ledger *Ledger,
	identity Identity,
) (Result, error) {
	ledger.RunTerminal = false
	if err := dispatcher.transition(ledger, StatusDispatching, ""); err != nil {
		return dispatcher.result(ledger), err
	}
	receipt, err := dispatcher.deps.GitHub.DispatchWorkflow(
		ctx, request.Repository, policy.Workflow, policy.TrustedRef,
		map[string]string{
			"candidate_sha": request.CandidateSHA, "candidate_ref": request.CandidateRef,
			"dispatch_nonce": identity.Nonce, "runner_label": identity.Label,
		},
	)
	if err != nil {
		if !errors.Is(err, ErrAmbiguous) {
			ledger.RunTerminal = true
		}
		return dispatcher.finishError(ledger, "dispatch", err)
	}
	if receipt.RunID <= 0 {
		return dispatcher.finishError(ledger, "dispatch_identity", fmt.Errorf("%w: dispatch returned no run ID", ErrAmbiguous))
	}
	ledger.RunID = receipt.RunID
	if err := dispatcher.transition(ledger, StatusWorkflowDispatched, ""); err != nil {
		return dispatcher.finishError(ledger, "state_dispatch", fmt.Errorf("%w: persist dispatch identity", ErrAmbiguous))
	}
	return dispatcher.bindAndRun(ctx, request, policy, ledger, identity)
}

func (dispatcher *Dispatcher) bindAndRun(
	ctx context.Context,
	request Request,
	policy RepositoryPolicy,
	ledger *Ledger,
	identity Identity,
) (Result, error) {
	run, err := dispatcher.waitForRun(ctx, request.Repository, policy, ledger, identity)
	if err != nil {
		return dispatcher.finishError(ledger, "run_bind", err)
	}
	job, err := dispatcher.waitForJob(ctx, request.Repository, policy, run.ID, identity.Label)
	if err != nil {
		return dispatcher.finishError(ledger, "job_bind", err)
	}
	ledger.JobID = job.ID
	if err := dispatcher.transition(ledger, StatusRunBound, ""); err != nil {
		return dispatcher.finishError(ledger, "state_run_bound", fmt.Errorf("%w: persist bound job", ErrAmbiguous))
	}
	collision, err := dispatcher.deps.GitHub.RunnerLabelExists(ctx, request.Repository, identity.Label)
	if err != nil {
		return dispatcher.finishError(ledger, "label_recheck", err)
	}
	if collision {
		return dispatcher.finishError(ledger, "label_race", fmt.Errorf("%w: runner label appeared after dispatch", ErrAmbiguous))
	}
	ledger.RunnerGone = false
	if err := dispatcher.deps.Store.Save(*ledger); err != nil {
		return dispatcher.finishError(ledger, "state_before_jit", fmt.Errorf("%w: persist JIT intent", ErrAmbiguous))
	}
	registration, err := dispatcher.deps.GitHub.GenerateJIT(ctx, request.Repository, identity, policy.RunnerGroupID)
	if err != nil {
		if authoritativeJITRejection(err) {
			ledger.RunnerGone = true
			if saveErr := dispatcher.deps.Store.Save(*ledger); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("%w: persist authoritative JIT rejection: %v", ErrAmbiguous, saveErr))
			}
		}
		return dispatcher.finishError(ledger, "jit", err)
	}
	if err := validateJITRegistration(registration, identity); err != nil {
		if registration.Secret != nil {
			registration.Secret.Zero()
		}
		return dispatcher.finishError(ledger, "jit_identity", err)
	}
	defer registration.Secret.Zero()
	ledger.RunnerID = registration.Runner.ID
	if err := dispatcher.transition(ledger, StatusJITCreated, ""); err != nil {
		return dispatcher.finishError(ledger, "state_jit", fmt.Errorf("%w: persist JIT runner identity", ErrAmbiguous))
	}
	return dispatcher.run(ctx, request, policy, ledger, identity, registration.Secret)
}

func authoritativeJITRejection(err error) bool {
	var remote *HTTPError
	return !errors.Is(err, ErrAmbiguous) && errors.As(err, &remote) && remote.Status >= 400 && remote.Status < 500
}

func (dispatcher *Dispatcher) run(
	ctx context.Context,
	request Request,
	policy RepositoryPolicy,
	ledger *Ledger,
	identity Identity,
	secret *JITSecret,
) (Result, error) {
	logDirectory, err := dispatcher.deps.Store.RequestLogDir(ledger.RequestID)
	if err != nil {
		return dispatcher.finishError(ledger, "log_directory", err)
	}
	ledger.IsolationGone = false
	if err := dispatcher.deps.Store.Save(*ledger); err != nil {
		return dispatcher.finishError(ledger, "state_before_isolation", fmt.Errorf("%w: persist isolation intent", ErrAmbiguous))
	}
	runCtx, cancel := context.WithTimeout(ctx, dispatcher.config.RunTimeout)
	defer cancel()
	sensitive := append([][]byte(nil), dispatcher.deps.Sensitive...)
	sensitive = append(sensitive, secret.Bytes())
	outcome, err := dispatcher.deps.Runner.Run(runCtx, RunnerRequest{
		DriverExecutable: dispatcher.config.IsolationDriver,
		DriverSHA256:     dispatcher.config.DriverSHA256,
		BaseImageSHA256:  dispatcher.config.BaseImageSHA256,
		RunnerDirectory:  dispatcher.config.RunnerDirectory,
		Identity:         identity, LeaseID: ledger.LeaseID, JITConfig: secret.Bytes(),
		LogDirectory: logDirectory, ShutdownGrace: dispatcher.config.ShutdownGrace,
		Sensitive: sensitive,
		OnStarted: func(process ProcessIdentity) error {
			ledger.ProcessPID = process.PID
			ledger.ProcessStart = process.StartTicks
			ledger.ProcessGroup = process.ProcessGroup
			ledger.CgroupPath = process.CgroupPath
			return dispatcher.transition(ledger, StatusRunnerStarted, "")
		},
		OnReady: func(receipt IsolationReceipt) error {
			ledger.IsolationID = receipt.IsolationID
			ledger.IsolationBase = receipt.BaseSHA256
			return dispatcher.transition(ledger, StatusIsolationReady, "")
		},
	})
	expectedProcess := ProcessIdentity{
		PID: ledger.ProcessPID, StartTicks: ledger.ProcessStart,
		ProcessGroup: ledger.ProcessGroup, CgroupPath: ledger.CgroupPath,
	}
	if err == nil && (outcome.Process != expectedProcess ||
		!validReadyReceipt(outcome.Ready, ledger.LeaseID, dispatcher.config.BaseImageSHA256) ||
		ledger.IsolationID != outcome.Ready.IsolationID || ledger.IsolationBase != outcome.Ready.BaseSHA256 ||
		!validDestroyedReceipt(outcome.Destroyed, ledger.LeaseID, ledger.IsolationID, dispatcher.config.BaseImageSHA256)) {
		err = fmt.Errorf("%w: runner lifecycle proof does not match durable state", ErrAmbiguous)
	}
	if err == nil {
		ledger.IsolationGone = true
		ledger.ProcessPID = 0
		ledger.ProcessStart = 0
		ledger.ProcessGroup = 0
		ledger.CgroupPath = ""
		if saveErr := dispatcher.deps.Store.Save(*ledger); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
	}
	if err != nil {
		return dispatcher.finishError(ledger, "runner", err)
	}
	if err := dispatcher.waitForCompletion(runCtx, request.Repository, ledger, policy, identity); err != nil {
		return dispatcher.finishError(ledger, "run_completion", err)
	}
	ledger.RunTerminal = true
	if err := dispatcher.ensureRunnerGone(runCtx, ledger); err != nil {
		return dispatcher.finishError(ledger, "runner_disappearance", err)
	}
	ledger.RunnerGone = true
	candidateSHA, err := dispatcher.deps.GitHub.ResolveRef(ctx, request.Repository, request.CandidateRef)
	if err != nil {
		return dispatcher.finishError(ledger, "candidate_revalidate", fmt.Errorf("%w: candidate ref could not be revalidated", ErrAmbiguous))
	}
	if candidateSHA != request.CandidateSHA {
		ledger.CleanupComplete = ledger.RunTerminal && ledger.RunnerGone && ledger.IsolationGone
		transitionErr := dispatcher.transition(ledger, StatusStale, "candidate_ref_moved")
		return dispatcher.result(ledger), errors.Join(fmt.Errorf("%w", ErrStale), transitionErr)
	}
	ledger.CleanupComplete = ledger.RunTerminal && ledger.RunnerGone && ledger.IsolationGone
	if !ledger.CleanupComplete {
		return dispatcher.finishError(ledger, "cleanup_proof", fmt.Errorf("%w: cleanup proof is incomplete", ErrAmbiguous))
	}
	if err := dispatcher.transition(ledger, StatusCompleted, ""); err != nil {
		ledger.CleanupComplete = false
		return dispatcher.result(ledger), fmt.Errorf("%w: persist completed run: %v", ErrAmbiguous, err)
	}
	dispatcher.log(ledger, "dispatch completed")
	return dispatcher.result(ledger), nil
}

func (dispatcher *Dispatcher) waitForRun(
	ctx context.Context,
	repository string,
	policy RepositoryPolicy,
	ledger *Ledger,
	identity Identity,
) (WorkflowRun, error) {
	bindCtx, cancel := context.WithTimeout(ctx, dispatcher.config.JobBindTimeout)
	defer cancel()
	for {
		run, err := dispatcher.deps.GitHub.GetRun(bindCtx, repository, ledger.RunID)
		if err == nil {
			if err := validateBoundRun(run, policy, ledger.TrustedSHA, identity.Nonce); err != nil {
				return WorkflowRun{}, err
			}
			return run, nil
		}
		if !IsHTTPStatus(err, 404) {
			return WorkflowRun{}, err
		}
		if err := dispatcher.deps.Sleep(bindCtx, dispatcher.config.JobPollInterval); err != nil {
			return WorkflowRun{}, fmt.Errorf("%w: run ID was not observable before timeout", ErrAmbiguous)
		}
	}
}

func validateBoundRun(run WorkflowRun, policy RepositoryPolicy, trustedSHA, nonce string) error {
	if err := validateRunIdentity(run, policy, trustedSHA, nonce); err != nil {
		return err
	}
	if (run.Status != "queued" && run.Status != "in_progress") || run.Conclusion != "" {
		return fmt.Errorf("run initial state is invalid")
	}
	return nil
}

func validateRunIdentity(run WorkflowRun, policy RepositoryPolicy, trustedSHA, nonce string) error {
	expectedPath := policy.Workflow + "@" + strings.TrimPrefix(policy.TrustedRef, "refs/heads/")
	if run.Event != "workflow_dispatch" || run.HeadSHA != trustedSHA ||
		run.DisplayTitle != "CIVM JIT "+nonce || run.Path != expectedPath {
		return fmt.Errorf("run identity does not match trusted dispatch")
	}
	return nil
}

func (dispatcher *Dispatcher) waitForJob(
	ctx context.Context,
	repository string,
	policy RepositoryPolicy,
	runID int64,
	label string,
) (WorkflowJob, error) {
	bindCtx, cancel := context.WithTimeout(ctx, dispatcher.config.JobBindTimeout)
	defer cancel()
	for {
		jobs, err := dispatcher.deps.GitHub.ListJobs(bindCtx, repository, runID)
		if err != nil {
			return WorkflowJob{}, err
		}
		if len(jobs) == 1 {
			job := jobs[0]
			if job.ID <= 0 || (job.RunID != 0 && job.RunID != runID) || job.Name != policy.JobName ||
				job.Status != "queued" || job.Conclusion != "" || len(job.Labels) != 1 || job.Labels[0] != label {
				return WorkflowJob{}, fmt.Errorf("queued job identity is invalid")
			}
			return job, nil
		}
		if len(jobs) > 1 {
			return WorkflowJob{}, fmt.Errorf("%w: dispatch created duplicate jobs", ErrAmbiguous)
		}
		if err := dispatcher.deps.Sleep(bindCtx, dispatcher.config.JobPollInterval); err != nil {
			return WorkflowJob{}, fmt.Errorf("%w: job was not observable before timeout", ErrAmbiguous)
		}
	}
}

func (dispatcher *Dispatcher) waitForCompletion(
	ctx context.Context,
	repository string,
	ledger *Ledger,
	policy RepositoryPolicy,
	identity Identity,
) error {
	for {
		run, err := dispatcher.deps.GitHub.GetRun(ctx, repository, ledger.RunID)
		if err != nil {
			return err
		}
		if err := validateRunIdentity(run, policy, ledger.TrustedSHA, identity.Nonce); err != nil {
			return fmt.Errorf("%w: bound run identity changed", ErrAmbiguous)
		}
		job, err := dispatcher.deps.GitHub.GetJob(ctx, repository, ledger.JobID)
		if err != nil {
			return err
		}
		if err := validateExecutedJob(job, ledger, policy, identity); err != nil {
			return err
		}
		if run.Status == "completed" && run.Conclusion != "success" {
			return fmt.Errorf("workflow completed without success")
		}
		if job.Status == "completed" && job.Conclusion != "success" {
			return fmt.Errorf("job completed without success")
		}
		if run.Status == "completed" && job.Status == "completed" {
			return nil
		}
		if !knownExecutionStatus(run.Status) || !knownExecutionStatus(job.Status) {
			return fmt.Errorf("workflow or job returned an unknown state")
		}
		if err := dispatcher.deps.Sleep(ctx, dispatcher.config.JobPollInterval); err != nil {
			return err
		}
	}
}

func knownExecutionStatus(status string) bool {
	return status == "completed" || nonterminalStatus(status)
}

func validateExecutedJob(job WorkflowJob, ledger *Ledger, policy RepositoryPolicy, identity Identity) error {
	if job.ID != ledger.JobID || job.RunID != ledger.RunID || job.Name != policy.JobName || len(job.Labels) != 1 || job.Labels[0] != identity.Label {
		return fmt.Errorf("%w: executed job identity changed", ErrAmbiguous)
	}
	if job.Status == "in_progress" || job.Status == "completed" {
		if job.RunnerID != ledger.RunnerID || job.RunnerName != identity.RunnerName || job.RunnerGroupID != policy.RunnerGroupID {
			return fmt.Errorf("%w: expected job did not execute on the generated JIT runner", ErrAmbiguous)
		}
	}
	return nil
}

func nonterminalStatus(status string) bool {
	switch status {
	case "queued", "in_progress", "pending", "waiting", "requested":
		return true
	default:
		return false
	}
}

func validateJITRegistration(registration JITRegistration, identity Identity) error {
	if registration.Secret == nil || len(registration.Secret.Bytes()) < 16 || len(registration.Secret.Bytes()) > maxJITResponse {
		return fmt.Errorf("%w: JIT secret is missing or invalid", ErrAmbiguous)
	}
	runner := registration.Runner
	if runner.ID <= 0 || runner.Name != identity.RunnerName || runner.Status != "offline" || runner.Busy {
		return fmt.Errorf("%w: JIT runner identity is invalid", ErrAmbiguous)
	}
	custom := 0
	for _, label := range runner.Labels {
		if label.Type == "custom" && label.Name == identity.Label {
			custom++
			continue
		}
		if label.Type != "read-only" || !knownDefaultRunnerLabel(label.Name) {
			return fmt.Errorf("%w: JIT runner labels are invalid", ErrAmbiguous)
		}
	}
	if custom != 1 {
		return fmt.Errorf("%w: exact JIT runner label is missing or duplicated", ErrAmbiguous)
	}
	return nil
}

func (dispatcher *Dispatcher) finishWithoutLease(ledger *Ledger, code string, cause error) (Result, error) {
	status := StatusFailed
	if errors.Is(cause, ErrAmbiguous) {
		status = StatusAmbiguous
	}
	transitionErr := dispatcher.transition(ledger, status, code)
	return dispatcher.result(ledger), errors.Join(cause, transitionErr)
}

func (dispatcher *Dispatcher) finishError(ledger *Ledger, code string, cause error) (Result, error) {
	status := StatusFailed
	if errors.Is(cause, ErrAmbiguous) {
		status = StatusAmbiguous
	}
	if transitionErr := dispatcher.transition(ledger, status, code); transitionErr != nil {
		cause = errors.Join(cause, transitionErr)
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), dispatcher.config.RecoveryTimeout)
	cleanupErr := dispatcher.cleanupLedger(recoveryCtx, ledger)
	cancel()
	if cleanupErr != nil {
		ledger.CleanupComplete = false
		cause = errors.Join(cause, fmt.Errorf("%w: cleanup remains unresolved: %v", ErrAmbiguous, cleanupErr))
		if ledger.Status != StatusAmbiguous {
			cause = errors.Join(cause, dispatcher.transition(ledger, StatusAmbiguous, "cleanup_unresolved"))
		}
	}
	return dispatcher.result(ledger), cause
}

func (dispatcher *Dispatcher) cleanupLedger(ctx context.Context, ledger *Ledger) error {
	logDirectory, err := dispatcher.deps.Store.RequestLogDir(ledger.RequestID)
	if err != nil {
		return err
	}
	if !ledger.IsolationGone {
		receipt, err := dispatcher.deps.Runner.Recover(ctx, RecoveryRequest{
			DriverExecutable: dispatcher.config.IsolationDriver,
			DriverSHA256:     dispatcher.config.DriverSHA256,
			BaseImageSHA256:  dispatcher.config.BaseImageSHA256,
			RunnerDirectory:  dispatcher.config.RunnerDirectory,
			LeaseID:          ledger.LeaseID, IsolationID: ledger.IsolationID,
			Process: ProcessIdentity{PID: ledger.ProcessPID, StartTicks: ledger.ProcessStart,
				ProcessGroup: ledger.ProcessGroup, CgroupPath: ledger.CgroupPath},
			LogDirectory: logDirectory, ShutdownGrace: dispatcher.config.ShutdownGrace,
			Sensitive: dispatcher.deps.Sensitive,
		})
		if err != nil {
			return err
		}
		if !validDestroyedReceipt(
			receipt, ledger.LeaseID, ledger.IsolationID, dispatcher.config.BaseImageSHA256,
		) {
			return fmt.Errorf("%w: recovery proof does not match durable state", ErrAmbiguous)
		}
		if ledger.IsolationID == "" {
			ledger.IsolationID = receipt.IsolationID
			ledger.IsolationBase = receipt.BaseSHA256
		}
		ledger.IsolationGone = true
		ledger.ProcessPID = 0
		ledger.ProcessStart = 0
		ledger.ProcessGroup = 0
		ledger.CgroupPath = ""
		if err := dispatcher.deps.Store.Save(*ledger); err != nil {
			return err
		}
	}
	if !ledger.RunTerminal {
		if ledger.RunID == 0 {
			return fmt.Errorf("%w: dispatched run identity is unknown", ErrAmbiguous)
		}
		policy, ok := dispatcher.config.Policy(ledger.Repository)
		if !ok {
			return fmt.Errorf("cleanup repository policy is unavailable")
		}
		identity := Identity{
			Nonce: ledger.Nonce, Label: ledger.RunnerLabel,
			RunnerName: ledger.RunnerName, WorkFolder: ledger.WorkFolder,
		}
		if !validIdentity(identity) {
			return fmt.Errorf("cleanup runner identity is invalid")
		}
		if err := dispatcher.cancelAndWaitTerminal(ctx, ledger, policy, identity); err != nil {
			return err
		}
		ledger.RunTerminal = true
		if err := dispatcher.deps.Store.Save(*ledger); err != nil {
			return err
		}
	}
	if !ledger.RunnerGone {
		if err := dispatcher.ensureRunnerGone(ctx, ledger); err != nil {
			return err
		}
		ledger.RunnerGone = true
	}
	ledger.CleanupComplete = ledger.RunTerminal && ledger.RunnerGone && ledger.IsolationGone
	if !ledger.CleanupComplete {
		return fmt.Errorf("cleanup postconditions are incomplete")
	}
	ledger.UpdatedAt = dispatcher.deps.Now().UTC()
	return dispatcher.deps.Store.Save(*ledger)
}

func (dispatcher *Dispatcher) cancelAndWaitTerminal(
	ctx context.Context,
	ledger *Ledger,
	policy RepositoryPolicy,
	identity Identity,
) error {
	run, err := dispatcher.deps.GitHub.GetRun(ctx, ledger.Repository, ledger.RunID)
	if err != nil {
		return err
	}
	if err := validateRunIdentity(run, policy, ledger.TrustedSHA, identity.Nonce); err != nil {
		return fmt.Errorf("%w: cleanup run identity changed", ErrAmbiguous)
	}
	if !knownExecutionStatus(run.Status) {
		return fmt.Errorf("cleanup run returned unknown state %q", run.Status)
	}
	if run.Status != "completed" {
		cancelErr := dispatcher.deps.GitHub.CancelRun(ctx, ledger.Repository, ledger.RunID)
		if cancelErr != nil && !IsHTTPStatus(cancelErr, 409) && !errors.Is(cancelErr, ErrAmbiguous) {
			return cancelErr
		}
	}
	for {
		run, err = dispatcher.deps.GitHub.GetRun(ctx, ledger.Repository, ledger.RunID)
		if err != nil {
			return err
		}
		if err := validateRunIdentity(run, policy, ledger.TrustedSHA, identity.Nonce); err != nil {
			return fmt.Errorf("%w: cleanup run identity changed", ErrAmbiguous)
		}
		if run.Status == "completed" {
			if run.Conclusion == "" {
				return fmt.Errorf("terminal run has no conclusion")
			}
			return nil
		}
		if !nonterminalStatus(run.Status) {
			return fmt.Errorf("run cancellation returned unknown state %q", run.Status)
		}
		if err := dispatcher.deps.Sleep(ctx, dispatcher.config.JobPollInterval); err != nil {
			return err
		}
	}
}

func (dispatcher *Dispatcher) ensureRunnerGone(ctx context.Context, ledger *Ledger) error {
	if ledger.RunnerID == 0 {
		observeCtx, cancel := context.WithTimeout(ctx, dispatcher.config.JobBindTimeout)
		defer cancel()
		for ledger.RunnerID == 0 {
			runner, found, err := dispatcher.deps.GitHub.FindRunnerByLabel(observeCtx, ledger.Repository, ledger.RunnerLabel)
			if err != nil || found {
				if err != nil {
					if errors.Is(observeCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
						return fmt.Errorf("%w: JIT creation remains unproven after observation timeout", ErrAmbiguous)
					}
					return err
				}
				if runner.ID <= 0 || runner.Name != ledger.RunnerName || !runnerHasLabel(runner, ledger.RunnerLabel) {
					return fmt.Errorf("%w: exact runner label belongs to another identity", ErrAmbiguous)
				}
				ledger.RunnerID = runner.ID
				if err := dispatcher.deps.Store.Save(*ledger); err != nil {
					return err
				}
				break
			}
			if err := dispatcher.deps.Sleep(observeCtx, dispatcher.config.JobPollInterval); err != nil {
				if errors.Is(observeCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					return fmt.Errorf("%w: JIT creation remains unproven after observation timeout", ErrAmbiguous)
				}
				return err
			}
		}
	}
	deleted := false
	for {
		runner, found, err := dispatcher.deps.GitHub.GetRunner(ctx, ledger.Repository, ledger.RunnerID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if runner.ID != ledger.RunnerID || runner.Name != ledger.RunnerName || !runnerHasLabel(runner, ledger.RunnerLabel) {
			return fmt.Errorf("%w: runner identity changed before removal", ErrAmbiguous)
		}
		if !deleted {
			if err := dispatcher.deps.GitHub.DeleteRunner(ctx, ledger.Repository, ledger.RunnerID); err != nil {
				return err
			}
			deleted = true
		}
		if err := dispatcher.deps.Sleep(ctx, dispatcher.config.JobPollInterval); err != nil {
			return err
		}
	}
}

func runnerHasLabel(runner RemoteRunner, expected string) bool {
	count := 0
	for _, label := range runner.Labels {
		if label.Name == expected && label.Type == "custom" {
			count++
		}
	}
	return count == 1
}

func (dispatcher *Dispatcher) transition(ledger *Ledger, status Status, failureCode string) error {
	ledger.Status = status
	ledger.FailureCode = failureCode
	ledger.UpdatedAt = dispatcher.deps.Now().UTC()
	if err := dispatcher.deps.Store.Save(*ledger); err != nil {
		return fmt.Errorf("persist %s: %w", status, err)
	}
	dispatcher.log(ledger, "dispatcher state changed")
	return nil
}

func (dispatcher *Dispatcher) result(ledger *Ledger) Result {
	return Result{RequestID: ledger.RequestID, RunID: ledger.RunID, Status: ledger.Status}
}

func (dispatcher *Dispatcher) log(ledger *Ledger, message string) {
	duration := dispatcher.deps.Now().UTC().Sub(ledger.CreatedAt)
	if duration < 0 {
		duration = 0
	}
	dispatcher.deps.Logger.Info(message,
		"request_id", ledger.RequestID, "repository", ledger.Repository,
		"status", ledger.Status, "run_id", ledger.RunID, "job_id", ledger.JobID,
		"runner_id", ledger.RunnerID, "duration_ms", duration.Milliseconds(),
	)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
