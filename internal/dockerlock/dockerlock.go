// Package dockerlock serializes box-wide docker-heavy work across concurrent
// self-hosted runners via a single advisory flock plus a JSON heartbeat file.
//
// A holder acquires LockPath with flock(LOCK_EX|LOCK_NB) using linear backoff
// until WaitBudget, then writes HeartbeatPath (.hb) with its PID and the PID
// start-ticks read from /proc/<pid>/stat field 22. A background goroutine
// rewrites the heartbeat every HeartbeatEvery for the whole life of the holder
// process: HoldBudget is only an alarm signal (over_budget), never a reclaim
// trigger, so a long-but-alive --exec is never reclaimed under it (SPECv2
// DT-v2-1). Staleness — the only condition that lets another caller reclaim —
// is a dead PID, a mismatched pid_start_ticks (PID reuse), or a heartbeat that
// has not been refreshed for more than 3× HeartbeatEvery (SPECv2 DT-v2-3).
//
// Every side effect is injected (FlockFn/OpenFileFn/ReadFileFn/WriteFileFn/
// RemoveFn/NowFn/PidAliveFn/PidStartTicksFn) so unit tests run with no real
// syscall, flock, exec, /proc or network access. The package imports only civm
// and the standard library; it must never import internal/capacity (SPECv2
// DT-v2-13).
package dockerlock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/procstat"
)

// defaultScope is the only enumerated scope; it is an observability label, the
// lock itself is a single global file (SPECv2 DT-v2-17).
const defaultScope = "docker-heavy"

// heartbeatFileMode keeps the .hb owner-rw, group-r, no world (SPECv2 DT-v2-18).
const heartbeatFileMode os.FileMode = 0o640

// runDirMode is the mode used to create the parent dir of LockPath (e.g.
// /run/civm) when it is missing.
const runDirMode os.FileMode = 0o755

// backoffStep is the linear backoff base between acquisition attempts; jitter
// of ±backoffJitter is added so concurrent waiters do not lock-step (SPECv2
// DT-v2-4: linear 100ms + jitter ±10ms, no exponential).
const (
	backoffStep   = 100 * time.Millisecond
	backoffJitter = 10 * time.Millisecond
)

// staleHeartbeatMultiplier defines how many HeartbeatEvery intervals may elapse
// without a heartbeat refresh before the holder is considered stale (the
// heartbeat goroutine died/stalled) — SPECv2 DT-v2-1.
const staleHeartbeatMultiplier = 3

// ErrWaitBudgetExceeded is returned by Acquire when the lock could not be taken
// within WaitBudget.
var ErrWaitBudgetExceeded = errors.New("docker-heavy lock wait budget exceeded")

// Heartbeat is the JSON payload persisted in HeartbeatPath. PIDStartTicks pins
// the holder identity so a reused PID does not look alive (SPECv2 DT-v2-3).
type Heartbeat struct {
	PID           int       `json:"pid"`
	PIDStartTicks uint64    `json:"pid_start_ticks"`
	Scope         string    `json:"scope"`
	AcquiredAt    time.Time `json:"acquired_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Repo          string    `json:"repo,omitempty"`
	RunID         string    `json:"run_id,omitempty"`
}

// Options configures Acquire/IsActive. Every effectful dependency is injected
// so tests need no real syscall/flock/exec/proc.
type Options struct {
	// LockPath is the advisory flock file (default DefaultDockerHeavyLockPath).
	LockPath string
	// HeartbeatPath is the JSON heartbeat file (default LockPath + ".hb").
	HeartbeatPath string
	// Scope is an observability label; only defaultScope is enumerated.
	Scope string
	// WaitBudget caps how long Acquire backs off before ErrWaitBudgetExceeded.
	WaitBudget time.Duration
	// HoldBudget is the over_budget alarm threshold; it never triggers reclaim.
	HoldBudget time.Duration
	// HeartbeatEvery is the heartbeat rewrite interval.
	HeartbeatEvery time.Duration
	// Repo / RunID are recorded in the heartbeat for observability only.
	Repo  string
	RunID string

	NowFn           func() time.Time
	FlockFn         func(fd int, how int) error
	OpenFileFn      func(name string, flag int, perm os.FileMode) (*os.File, error)
	ReadFileFn      func(name string) ([]byte, error)
	WriteFileFn     func(name string, data []byte, perm os.FileMode) error
	RemoveFn        func(name string) error
	MkdirAllFn      func(path string, perm os.FileMode) error
	PidAliveFn      func(pid int) bool
	PidStartTicksFn func(pid int) (uint64, error)
}

// Lock is an acquired docker-heavy lock. Release stops the heartbeat goroutine,
// unlocks and removes the heartbeat file. It is safe to call Release more than
// once.
type Lock struct {
	opts      Options
	file      *os.File
	scope     string
	acquireAt time.Time
	waited    time.Duration

	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	released bool
	over     bool // over_budget: HoldBudget crossed while still alive
}

// DefaultOptions returns production wiring.
func DefaultOptions() Options {
	return Options{
		LockPath:        civm.DefaultDockerHeavyLockPath,
		HeartbeatPath:   civm.DefaultDockerHeavyLockPath + ".hb",
		Scope:           defaultScope,
		WaitBudget:      time.Duration(civm.DefaultDockerHeavyLockWaitMinutes) * time.Minute,
		HoldBudget:      time.Duration(civm.DefaultDockerHeavyLockBudgetMinutes) * time.Minute,
		HeartbeatEvery:  time.Duration(civm.DefaultDockerHeavyHeartbeatSeconds) * time.Second,
		NowFn:           time.Now,
		FlockFn:         syscall.Flock,
		OpenFileFn:      os.OpenFile,
		ReadFileFn:      os.ReadFile,
		WriteFileFn:     os.WriteFile,
		RemoveFn:        os.Remove,
		MkdirAllFn:      os.MkdirAll,
		PidAliveFn:      defaultPidAlive,
		PidStartTicksFn: procstat.PidStartTicks,
	}
}

func applyDefaults(opts *Options) {
	if opts.LockPath == "" {
		opts.LockPath = civm.DefaultDockerHeavyLockPath
	}
	if opts.HeartbeatPath == "" {
		opts.HeartbeatPath = opts.LockPath + ".hb"
	}
	if opts.Scope == "" {
		opts.Scope = defaultScope
	}
	if opts.WaitBudget <= 0 {
		opts.WaitBudget = time.Duration(civm.DefaultDockerHeavyLockWaitMinutes) * time.Minute
	}
	if opts.HoldBudget <= 0 {
		opts.HoldBudget = time.Duration(civm.DefaultDockerHeavyLockBudgetMinutes) * time.Minute
	}
	if opts.HeartbeatEvery <= 0 {
		opts.HeartbeatEvery = time.Duration(civm.DefaultDockerHeavyHeartbeatSeconds) * time.Second
	}
	if opts.NowFn == nil {
		opts.NowFn = time.Now
	}
	if opts.FlockFn == nil {
		opts.FlockFn = syscall.Flock
	}
	if opts.OpenFileFn == nil {
		opts.OpenFileFn = os.OpenFile
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = os.ReadFile
	}
	if opts.WriteFileFn == nil {
		opts.WriteFileFn = os.WriteFile
	}
	if opts.RemoveFn == nil {
		opts.RemoveFn = os.Remove
	}
	if opts.MkdirAllFn == nil {
		opts.MkdirAllFn = os.MkdirAll
	}
	if opts.PidAliveFn == nil {
		opts.PidAliveFn = defaultPidAlive
	}
	if opts.PidStartTicksFn == nil {
		opts.PidStartTicksFn = procstat.PidStartTicks
	}
}

// Acquire takes the docker-heavy lock, retrying with linear backoff (100ms ±
// 10ms jitter) until WaitBudget elapses. On success it writes the heartbeat and
// starts a goroutine that rewrites it every HeartbeatEvery for the life of the
// holder. It returns ErrWaitBudgetExceeded if the budget is exhausted.
func Acquire(ctx context.Context, opts Options) (*Lock, error) {
	applyDefaults(&opts)
	if err := validateAbsPath(opts.LockPath, "lock-path"); err != nil {
		return nil, err
	}
	if err := opts.MkdirAllFn(filepath.Dir(opts.LockPath), runDirMode); err != nil {
		return nil, fmt.Errorf("dockerlock: criar dir %s: %w", filepath.Dir(opts.LockPath), err)
	}

	start := opts.NowFn()
	deadline := start.Add(opts.WaitBudget)
	for {
		lock, err := tryAcquireOnce(ctx, opts)
		if err != nil {
			return nil, err
		}
		if lock != nil {
			lock.waited = opts.NowFn().Sub(start)
			return lock, nil
		}
		if !opts.NowFn().Before(deadline) {
			return nil, ErrWaitBudgetExceeded
		}
		if err := sleepCtx(ctx, nextBackoff()); err != nil {
			return nil, err
		}
	}
}

// tryAcquireOnce attempts a single non-blocking flock. On success it returns a
// started Lock; on contention it returns (nil, nil); a real error is fatal.
func tryAcquireOnce(ctx context.Context, opts Options) (*Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := opts.OpenFileFn(opts.LockPath, os.O_CREATE|os.O_RDWR, heartbeatFileMode)
	if err != nil {
		return nil, fmt.Errorf("dockerlock: abrir %s: %w", opts.LockPath, err)
	}
	if err := opts.FlockFn(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if isContention(err) {
			// Held by another holder. If the heartbeat is stale (dead PID /
			// reused PID / not refreshed in > 3× HeartbeatEvery) we drop it so
			// the next backoff iteration can take the lock. A fresh holder is
			// left untouched. Reclaim failures are not fatal: keep waiting.
			_, _ = reclaimStale(opts)
			return nil, nil
		}
		return nil, fmt.Errorf("dockerlock: flock %s: %w", opts.LockPath, err)
	}

	now := opts.NowFn()
	l := &Lock{
		opts:      opts,
		file:      f,
		scope:     opts.Scope,
		acquireAt: now,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if err := l.writeHeartbeat(now); err != nil {
		_ = l.unlockAndClose()
		return nil, err
	}
	go l.heartbeatLoop()
	return l, nil
}

// WaitedMS returns how long Acquire blocked before taking the lock.
func (l *Lock) WaitedMS() int64 { return l.waited.Milliseconds() }

// Scope returns the observability scope label of the acquired lock.
func (l *Lock) Scope() string { return l.scope }

// OverBudget reports whether the holder crossed HoldBudget while still alive.
func (l *Lock) OverBudget() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.over
}

// HoldMS returns how long the lock has been held so far.
func (l *Lock) HoldMS() int64 {
	return l.opts.NowFn().Sub(l.acquireAt).Milliseconds()
}

// Release stops the heartbeat goroutine, unlocks the flock, closes the fd and
// removes the heartbeat file. It is idempotent.
func (l *Lock) Release() error {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	l.mu.Unlock()

	close(l.stop)
	<-l.done

	var firstErr error
	if err := l.opts.RemoveFn(l.opts.HeartbeatPath); err != nil && !os.IsNotExist(err) {
		firstErr = fmt.Errorf("dockerlock: remover heartbeat %s: %w", l.opts.HeartbeatPath, err)
	}
	if err := l.unlockAndClose(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (l *Lock) unlockAndClose() error {
	ferr := l.opts.FlockFn(int(l.file.Fd()), syscall.LOCK_UN)
	cerr := l.file.Close()
	if ferr != nil {
		return fmt.Errorf("dockerlock: unlock %s: %w", l.opts.LockPath, ferr)
	}
	if cerr != nil {
		return fmt.Errorf("dockerlock: fechar %s: %w", l.opts.LockPath, cerr)
	}
	return nil
}

// heartbeatLoop rewrites the heartbeat every HeartbeatEvery until Release. It
// keeps refreshing past HoldBudget (never abandons a live holder) and only
// flips the over_budget alarm once HoldBudget is crossed.
func (l *Lock) heartbeatLoop() {
	defer close(l.done)
	ticker := time.NewTicker(l.opts.HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			now := l.opts.NowFn()
			if now.Sub(l.acquireAt) >= l.opts.HoldBudget {
				l.mu.Lock()
				l.over = true
				l.mu.Unlock()
			}
			// Best-effort: a failed refresh is logged by the caller layer;
			// staleness recovery handles a permanently dead heartbeat.
			_ = l.writeHeartbeat(now)
		}
	}
}

func (l *Lock) writeHeartbeat(now time.Time) error {
	startTicks, err := l.opts.PidStartTicksFn(os.Getpid())
	if err != nil {
		return fmt.Errorf("dockerlock: ler pid start-ticks: %w", err)
	}
	hb := Heartbeat{
		PID:           os.Getpid(),
		PIDStartTicks: startTicks,
		Scope:         l.scope,
		AcquiredAt:    l.acquireAt,
		ExpiresAt:     now.Add(staleHeartbeatMultiplier * l.opts.HeartbeatEvery),
		Repo:          l.opts.Repo,
		RunID:         l.opts.RunID,
	}
	data, err := json.Marshal(hb)
	if err != nil {
		return fmt.Errorf("dockerlock: serializar heartbeat: %w", err)
	}
	if err := l.opts.WriteFileFn(l.opts.HeartbeatPath, data, heartbeatFileMode); err != nil {
		return fmt.Errorf("dockerlock: gravar heartbeat %s: %w", l.opts.HeartbeatPath, err)
	}
	return nil
}

// IsActive reports whether a fresh, live holder currently owns the lock. It is
// true iff the heartbeat unmarshals, the PID is alive, the recorded
// pid_start_ticks matches the live process, and the heartbeat has not expired
// (refreshed within staleHeartbeatMultiplier × HeartbeatEvery). Any other case
// — missing/corrupt heartbeat, dead PID, mismatched start-ticks, expired — is
// reported as not active (stale).
func IsActive(opts Options) (bool, error) {
	applyDefaults(&opts)
	hb, ok, err := readHeartbeat(opts)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return isFresh(opts, hb), nil
}

// Holder returns a compact "<repo>#<runId>" identity from the current
// heartbeat, or "" when there is no fresh holder. It never errors; IO problems
// resolve to "".
func Holder(opts Options) string {
	applyDefaults(&opts)
	hb, ok, err := readHeartbeat(opts)
	if err != nil || !ok || !isFresh(opts, hb) {
		return ""
	}
	switch {
	case hb.Repo != "" && hb.RunID != "":
		return hb.Repo + "#" + hb.RunID
	case hb.Repo != "":
		return hb.Repo
	default:
		return strconv.Itoa(hb.PID)
	}
}

func isFresh(opts Options, hb Heartbeat) bool {
	if !opts.PidAliveFn(hb.PID) {
		return false
	}
	startTicks, err := opts.PidStartTicksFn(hb.PID)
	if err != nil || startTicks != hb.PIDStartTicks {
		return false
	}
	return opts.NowFn().Before(hb.ExpiresAt)
}

// reclaimStale removes the heartbeat file when it is provably stale (missing,
// corrupt, dead PID, mismatched start-ticks, or expired). It is idempotent and
// re-entrant: removing an already-absent file is success. A still-fresh holder
// is never reclaimed.
func reclaimStale(opts Options) (bool, error) {
	hb, ok, err := readHeartbeat(opts)
	if err != nil {
		return false, err
	}
	if ok && isFresh(opts, hb) {
		return false, nil
	}
	if err := opts.RemoveFn(opts.HeartbeatPath); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("dockerlock: reclaim heartbeat %s: %w", opts.HeartbeatPath, err)
	}
	return true, nil
}

// ReclaimStale is the exported wrapper used by cleanup/watchdog callers.
func ReclaimStale(opts Options) (bool, error) {
	applyDefaults(&opts)
	return reclaimStale(opts)
}

// readHeartbeat reads and unmarshals the heartbeat. A missing file is (zero,
// false, nil); a corrupt file is (zero, false, nil) — corruption is treated as
// stale, not as a hard error (SPECv2 DT-v2-3).
func readHeartbeat(opts Options) (Heartbeat, bool, error) {
	data, err := opts.ReadFileFn(opts.HeartbeatPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Heartbeat{}, false, nil
		}
		return Heartbeat{}, false, fmt.Errorf("dockerlock: ler heartbeat %s: %w", opts.HeartbeatPath, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Heartbeat{}, false, nil
	}
	var hb Heartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		// Corrupt heartbeat → stale, not fatal.
		return Heartbeat{}, false, nil
	}
	return hb, true, nil
}

func validateAbsPath(path, field string) error {
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("dockerlock: %s contem byte NUL", field)
	}
	if !filepath.IsAbs(filepath.Clean(path)) {
		return fmt.Errorf("dockerlock: %s deve ser caminho absoluto, got %q", field, path)
	}
	return nil
}

func nextBackoff() time.Duration {
	// G404: backoff jitter is timing-only desync between waiters, not security.
	jitter := time.Duration(rand.Int64N(int64(2*backoffJitter+1))) - backoffJitter //nolint:gosec
	d := backoffStep + jitter
	if d < 0 {
		d = backoffStep
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isContention reports whether a flock(LOCK_NB) error means the lock is held by
// another holder (vs. a real IO/programming error). On Linux flock(LOCK_NB)
// returns EWOULDBLOCK (== EAGAIN) when the lock is busy.
func isContention(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

// defaultPidAlive reports whether pid is alive via signal 0.
func defaultPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the process exists but is owned by another user.
	return errors.Is(err, syscall.EPERM)
}
