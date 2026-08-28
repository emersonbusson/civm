package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/emersonbusson/civm/internal/dockerlock"
)

// lockScopeDockerHeavy is the only enumerated --scope value (SPECv2 DT-v2-17).
const lockScopeDockerHeavy = "docker-heavy"

// lockEventAcquire / lockEventRelease / lockEventWaitExceeded are the
// hooks.jsonl event names (SPECv2 §Observabilidade v2). Emission is best-effort
// to stderr and never changes the exit code.
const (
	lockEventAcquire      = "lock_acquire"
	lockEventRelease      = "lock_release"
	lockEventWaitExceeded = "lock_wait_budget_exceeded"
)

// lockFlags holds the parsed CLI surface shared by every lock subform.
type lockFlags struct {
	scope   string
	budget  time.Duration
	wait    time.Duration
	repo    string
	runID   string
	jsonOut bool
}

// runLock dispatches the lock command. The primary form is --exec, which
// acquires, runs the inner command with the lock held (refreshing the
// heartbeat for the whole command lifetime), then releases. acquire/release
// subforms exist for scripted callers that manage their own lifecycle.
func runLock(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "acquire":
			return runLockAcquire(args[1:])
		case "release":
			return runLockRelease(args[1:])
		}
	}
	return runLockExec(args)
}

// parseLockFlags wires the flags common to all lock subforms. It returns the
// flag set so callers can read positional --exec args via fs.Args().
func parseLockFlags(name string, args []string, lf *lockFlags, withExec bool) (*flag.FlagSet, error) {
	fs := flag.NewFlagSet("lock "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&lf.scope, "scope", lockScopeDockerHeavy, "rotulo de observabilidade (apenas docker-heavy)")
	fs.DurationVar(&lf.budget, "budget", 0, "HOLD budget: alarme over_budget (default 50m)")
	fs.DurationVar(&lf.wait, "wait", 0, "WAIT budget: falha alto se nao adquirir (default 75m)")
	fs.StringVar(&lf.repo, "repo", "", "repo holder (observabilidade)")
	fs.StringVar(&lf.runID, "run-id", "", "run id holder (observabilidade)")
	fs.BoolVar(&lf.jsonOut, "json", false, "emitir eventos do lock em JSON no stderr")
	if withExec {
		fs.Bool("exec", false, "executar o comando apos -- com o lock seguro")
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if lf.scope != lockScopeDockerHeavy {
		return nil, fmt.Errorf("--scope desconhecido %q (use %s)", lf.scope, lockScopeDockerHeavy)
	}
	return fs, nil
}

// buildLockOptions maps parsed flags onto dockerlock.Options, leaving
// zero-valued budgets to DefaultOptions.
func buildLockOptions(lf lockFlags) dockerlock.Options {
	opts := dockerlock.DefaultOptions()
	opts.Scope = lf.scope
	opts.Repo = lf.repo
	opts.RunID = lf.runID
	if lf.budget > 0 {
		opts.HoldBudget = lf.budget
	}
	if lf.wait > 0 {
		opts.WaitBudget = lf.wait
	}
	return opts
}

func runLockExec(args []string) int {
	var lf lockFlags
	fs, err := parseLockFlags("exec", args, &lf, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de lock:", err)
		return exitUsage
	}
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "uso: civmctl lock --exec --scope docker-heavy [--budget 50m] [--wait 75m] -- <cmd...>")
		return exitUsage
	}

	opts := buildLockOptions(lf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lock, err := dockerlock.Acquire(ctx, opts)
	if err != nil {
		if errors.Is(err, dockerlock.ErrWaitBudgetExceeded) {
			emitLockEvent(lf.jsonOut, lockEvent{
				Event:    lockEventWaitExceeded,
				Scope:    lf.scope,
				Repo:     lf.repo,
				RunID:    lf.runID,
				WaitedMS: lf.wait.Milliseconds(),
			})
			return exitLockWaitTimeout
		}
		fmt.Fprintln(os.Stderr, "erro ao adquirir lock:", err)
		return exitLockInternal
	}
	emitLockEvent(lf.jsonOut, lockEvent{
		Event:  lockEventAcquire,
		Scope:  lock.Scope(),
		Repo:   lf.repo,
		RunID:  lf.runID,
		WaitMS: lock.WaitedMS(),
		PID:    os.Getpid(),
	})

	// Release on SIGTERM/SIGINT so a graceful stop never leaks the lock. A
	// SIGKILL cannot run defers; stale-detection recovers within 3×heartbeat.
	released := make(chan struct{})
	releaseOnce := releaseFunc(lock, lf, released)
	defer releaseOnce()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			releaseOnce()
			cancel()
		case <-released:
		}
	}()

	code := runInnerCommand(ctx, cmdArgs)
	releaseOnce()
	return code
}

// releaseFunc returns an idempotent release that closes the released channel
// once and emits the lock_release event with the over_budget alarm.
func releaseFunc(lock *dockerlock.Lock, lf lockFlags, released chan struct{}) func() {
	var done bool
	return func() {
		if done {
			return
		}
		done = true
		holdMS := lock.HoldMS()
		over := lock.OverBudget()
		if err := lock.Release(); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao liberar lock:", err)
		}
		emitLockEvent(lf.jsonOut, lockEvent{
			Event:      lockEventRelease,
			Scope:      lf.scope,
			Repo:       lf.repo,
			RunID:      lf.runID,
			HoldMS:     holdMS,
			OverBudget: over,
			PID:        os.Getpid(),
		})
		close(released)
	}
}

// runInnerCommand runs the wrapped command with inherited stdio and returns its
// exit code (propagated on failure per SPECv2 §Exit codes).
func runInnerCommand(ctx context.Context, cmdArgs []string) int {
	//nolint:gosec // G204: docker-heavy command comes from the operator's own
	// shell after `--`; civmctl only wraps it under the lock.
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "erro ao executar comando:", err)
		return exitLockInternal
	}
	return 0
}

// runLockAcquire acquires the lock and immediately releases it: a scripted
// caller uses this to assert the lock is takeable. Holding across process
// boundaries is the job of --exec, which keeps the heartbeat alive.
func runLockAcquire(args []string) int {
	var lf lockFlags
	if _, err := parseLockFlags("acquire", args, &lf, false); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de lock acquire:", err)
		return exitUsage
	}
	opts := buildLockOptions(lf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lock, err := dockerlock.Acquire(ctx, opts)
	if err != nil {
		if errors.Is(err, dockerlock.ErrWaitBudgetExceeded) {
			emitLockEvent(lf.jsonOut, lockEvent{Event: lockEventWaitExceeded, Scope: lf.scope, Repo: lf.repo, RunID: lf.runID, WaitedMS: lf.wait.Milliseconds()})
			return exitLockWaitTimeout
		}
		fmt.Fprintln(os.Stderr, "erro ao adquirir lock:", err)
		return exitLockInternal
	}
	emitLockEvent(lf.jsonOut, lockEvent{Event: lockEventAcquire, Scope: lock.Scope(), Repo: lf.repo, RunID: lf.runID, WaitMS: lock.WaitedMS(), PID: os.Getpid()})
	if err := lock.Release(); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao liberar lock:", err)
		return exitLockInternal
	}
	emitLockEvent(lf.jsonOut, lockEvent{Event: lockEventRelease, Scope: lf.scope, Repo: lf.repo, RunID: lf.runID, PID: os.Getpid()})
	return 0
}

// runLockRelease reclaims a stale heartbeat for the scope. It never force-frees
// a live holder (reclaimStale leaves a fresh heartbeat untouched).
func runLockRelease(args []string) int {
	var lf lockFlags
	if _, err := parseLockFlags("release", args, &lf, false); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de lock release:", err)
		return exitUsage
	}
	opts := buildLockOptions(lf)
	if _, err := dockerlock.ReclaimStale(opts); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao reclamar lock stale:", err)
		return exitLockInternal
	}
	return 0
}

// lockEvent is the JSON shape emitted to stderr (SPECv2 §Observabilidade v2).
// omitempty keeps each event line minimal and PII-free.
type lockEvent struct {
	Timestamp  string `json:"timestamp"`
	Event      string `json:"event"`
	Scope      string `json:"scope,omitempty"`
	Repo       string `json:"repo,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	WaitMS     int64  `json:"wait_ms,omitempty"`
	WaitedMS   int64  `json:"waited_ms,omitempty"`
	HoldMS     int64  `json:"hold_ms,omitempty"`
	OverBudget bool   `json:"over_budget,omitempty"`
	PID        int    `json:"pid,omitempty"`
}

// emitLockEvent writes a single observability line to stderr. It is
// best-effort: a serialization/IO failure never alters the command outcome
// (SPECv2: "log para stderr e segue").
func emitLockEvent(jsonOut bool, ev lockEvent) {
	ev.Timestamp = time.Now().UTC().Format(time.RFC3339)
	if jsonOut {
		data, err := json.Marshal(ev)
		if err != nil {
			return
		}
		fmt.Fprintln(os.Stderr, string(data))
		return
	}
	fmt.Fprintf(os.Stderr, "lock %s scope=%s repo=%s run_id=%s wait_ms=%d hold_ms=%d over_budget=%t\n",
		ev.Event, ev.Scope, ev.Repo, ev.RunID, ev.WaitMS, ev.HoldMS, ev.OverBudget)
}
