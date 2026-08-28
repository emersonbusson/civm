// Package admit gates memory-heavy CI jobs through a fixed set of advisory
// flock slots so at most MaxHeavy heavy jobs run concurrently on the box, while
// light jobs flow unbounded. It is the core of `civmctl admit`
// (docs/specs/runner-memory-admission/SPECv3.md, ITEM-3).
//
// Mechanism (SPECv3 DT-v3-2/3/7):
//   - heavy → loop per attempt: (a) the watchdog gate (CheckFn): Critical/Warn
//     backs off (NO MemAvailable arithmetic — the cgroup + RAM invariant already
//     prove the fit, DT-v3-7); (b) reap-on-reuse: a free slot whose recorded
//     unit is orphaned (its admit PID dead) is stopped before reuse (DT-v3-2);
//     (c) a non-blocking flock on the first free slot. After WaitBudget →
//     ErrWaitBudgetExceeded, a typed timeout the CLI maps to a dedicated exit
//     code — NEVER a minted N+1 slot, never an infinite hang (DT-v3-3).
//   - light → returns immediately, holding no slot.
//   - Release stops the recorded systemd unit (co-termination, DT-v3-2) and
//     releases the slot flock; it is idempotent.
//
// Liveness is the flock itself (kernel-released on holder death); there is no
// heartbeat (DT-v3-2). A CheckFn error is fail-closed: it backs off and never
// admits (DT-v3-2/v2-2). Every effectful dependency is injected so unit tests
// run with no real flock, systemd-run/systemctl, /proc or clock. The package
// imports only memwatchdog/civm and the standard library.
package admit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/memwatchdog"
	"github.com/emersonbusson/civm/internal/procstat"
)

// Weight is the job memory weight: heavy jobs take a slot, light jobs do not.
type Weight string

const (
	// WeightHeavy jobs compete for one of MaxHeavy flock slots.
	WeightHeavy Weight = "heavy"
	// WeightLight jobs flow unbounded with no slot (SPECv3 ITEM-3).
	WeightLight Weight = "light"
)

// slotRecordMode keeps the slot record owner-rw, group-r, no world.
const slotRecordMode os.FileMode = 0o640

// ErrWaitBudgetExceeded is returned by Acquire when no heavy slot (or the docker
// sub-slot) could be taken within WaitBudget. The CLI maps it to a dedicated
// exit code; it NEVER mints an extra slot and never hangs (SPECv3 DT-v3-3).
var ErrWaitBudgetExceeded = errors.New("admit: heavy slot wait budget exceeded")

// slotRecord is the JSON persisted in each occupied slot file. It records the
// systemd unit (so Release/reap can `systemctl stop` it) and the admit PID +
// its /proc start-ticks (so reap-on-reuse can tell an orphaned holder from a
// live one, and a recycled PID from the original holder) — SPECv3 DT-v3-2.
type slotRecord struct {
	Unit          string    `json:"unit"`
	PID           int       `json:"pid"`
	PIDStartTicks uint64    `json:"pid_start_ticks"`
	AcquiredAt    time.Time `json:"acquired_at"`
}

// Options configures Acquire. Every effectful dependency is injected so tests
// need no real flock/systemd/proc/clock (SPECv3 §Plano de testes).
type Options struct {
	// MaxHeavy is the number of fixed heavy slots (the concurrency ceiling).
	MaxHeavy int
	// HeavyMaxMB is the calibrated per-job cap; 0 means generous (DT-v3-5). It is
	// carried for the CLI wrapper, not used by Acquire's gating.
	HeavyMaxMB int64
	// SlotPrefix + "{1..MaxHeavy}.lock" are the heavy slot flock paths.
	SlotPrefix string
	// DockerSlotPath is the count=1 docker sub-slot (DT-v3-8).
	DockerSlotPath string
	// Weight selects the path: heavy takes a slot, light returns immediately.
	Weight Weight
	// Exclusive, when "docker", also acquires the docker sub-slot (DT-v3-8).
	Exclusive string
	// WaitBudget caps total backoff before ErrWaitBudgetExceeded.
	WaitBudget time.Duration
	// Backoff is the sleep between attempts (linear; no exponential).
	Backoff time.Duration

	// FlockFn takes a non-blocking advisory lock on path, returning a release fn
	// on success or an error (contention or IO). Injected; the default wraps a
	// real flock(LOCK_EX|LOCK_NB) on an O_CREATE file descriptor.
	FlockFn func(path string) (release func(), err error)
	// CheckFn is the watchdog gate (memwatchdog.Check decision). An error is
	// fail-closed (backoff, never admit).
	CheckFn func() (memwatchdog.Decision, error)
	// RunFn runs systemctl (stop) for co-termination/reap. Injected.
	RunFn func(name string, args ...string) ([]byte, error)
	// PidAliveFn reports whether a recorded admit PID is still alive (orphan gate).
	PidAliveFn func(pid int) bool
	// PidStartTicksFn reads a PID's /proc start-ticks so reap-on-reuse can reject
	// a recycled PID whose start-time no longer matches the recorded holder
	// (PID-reuse defense, mirrors internal/dockerlock). Injected for tests.
	PidStartTicksFn func(pid int) (uint64, error)
	// ReadFileFn / WriteFileFn persist the slot record.
	ReadFileFn  func(path string) ([]byte, error)
	WriteFileFn func(path string, data []byte, perm uint32) error
	// NowFn / SleepFn drive the WaitBudget deadline and backoff.
	NowFn   func() time.Time
	SleepFn func(time.Duration)
}

// Admission is a granted admission. For heavy jobs it holds the slot flock and
// (after SetUnit) the recorded systemd unit. Release stops the unit and frees
// the slot; it is idempotent and safe to call from a signal handler.
type Admission struct {
	opts     Options
	slotPath string // "" for light admissions
	release  func() // slot flock release; nil for light
	unit     string // recorded systemd unit; "" until SetUnit
	docker   func() // docker sub-slot release; nil unless --exclusive=docker
	done     bool   // idempotency guard for Release
}

// HoldsSlot reports whether this admission occupies a heavy slot.
func (a *Admission) HoldsSlot() bool { return a.slotPath != "" }

// SlotPath returns the heavy slot path held, or "" for a light admission.
func (a *Admission) SlotPath() string { return a.slotPath }

// UnitName returns the deterministic transient-unit name this admission reserved
// (e.g. civm-admit-heavy-1-12345.service), or "" for a light admission. The CLI
// passes it to `systemd-run --unit=` so the unit name is known a priori and was
// persisted in the slot record BEFORE the payload started — a SIGKILLed admit
// still leaves a reapable record (DT-v3-2), with no stderr-scrape race.
func (a *Admission) UnitName() string { return a.unit }

// Release stops the recorded unit (if any) via `sudo systemctl stop` and releases
// the slot + docker flocks. It is idempotent: a second call is a no-op and the
// unit is stopped at most once (SPECv3 §Plano de testes). The slot flock release
// is the capacity-relevant effect and always runs; the unit stop is best-effort
// co-termination (DT-v3-2) so Release never reports a non-fatal error.
func (a *Admission) Release() error {
	if a.done {
		return nil
	}
	a.done = true
	stopUnit(a.opts, a.unit)
	if a.docker != nil {
		a.docker()
	}
	if a.release != nil {
		a.release()
	}
	return nil
}

// stopUnit co-terminates a transient unit best-effort (DT-v3-2). The job runs as
// a SYSTEM transient service, so the stop needs root (the runner has NOPASSWD
// sudo); and in the normal path `systemd-run --wait --collect` already let the
// unit finish and garbage-collected it, so `stop` reports "not loaded" — that is
// success, not an error. The stop only does real work when Release/reap runs
// while the job is still up (admit died mid-run). Callers ignore the result.
func stopUnit(opts Options, unit string) {
	if unit == "" || opts.RunFn == nil {
		return
	}
	// Validate the unit name before it reaches a root `systemctl` call, and pass
	// it after `--` so a hostile token can never be parsed as an option. The unit
	// is normally the deterministic civm-admit-* name, but defend in depth: a
	// corrupted/forged slot record must not inject args into a privileged verb.
	if err := civm.ValidateServiceUnit(unit); err != nil {
		slog.Warn("admit_stop_invalid_unit", "event", "admit_stop_invalid_unit", "unit", unit, "err", err.Error())
		return
	}
	if out, err := opts.RunFn("sudo", "systemctl", "stop", "--", unit); err != nil {
		slog.Debug("admit_stop_besteffort", "event", "admit_stop_besteffort",
			"unit", unit, "err", err.Error(), "out", strings.TrimSpace(string(out)))
	}
}

// unitNameFor derives a deterministic, validated transient-unit name from the
// held slot and the admit PID — e.g. /run/civm/admit-heavy-1.lock + pid 12345 ->
// "civm-admit-heavy-1-12345.service". Known a priori (no stderr scrape) and
// persisted before the payload starts so a crashed admit stays reapable (C1);
// the PID keeps it unique per holder. Falls back to a guaranteed-valid name if a
// non-conforming slot base would produce an invalid unit.
func unitNameFor(slotPath string, pid int) string {
	base := strings.TrimSuffix(filepath.Base(slotPath), ".lock")
	name := fmt.Sprintf("civm-%s-%d.service", base, pid)
	if civm.ValidateServiceUnit(name) != nil {
		return fmt.Sprintf("civm-admit-%d.service", pid)
	}
	return name
}

// Acquire grants an admission per Options.Weight. Light returns immediately with
// no slot. Heavy loops — watchdog gate, reap orphans, flock a free slot — until
// success or ErrWaitBudgetExceeded (SPECv3 ITEM-3, DT-v3-3/7).
func Acquire(ctx context.Context, opts Options) (*Admission, error) {
	applyDefaults(&opts)
	if err := validate(opts); err != nil {
		return nil, err
	}
	if opts.Weight == WeightLight {
		return &Admission{opts: opts}, nil
	}
	deadline := opts.NowFn().Add(opts.WaitBudget)
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("admit: %w", err)
		}
		adm, retry, err := tryAcquireHeavy(opts)
		if err != nil {
			return nil, err
		}
		if adm != nil {
			return adm, nil
		}
		if !retry {
			return nil, ErrWaitBudgetExceeded
		}
		if !opts.NowFn().Before(deadline) {
			return nil, ErrWaitBudgetExceeded
		}
		opts.SleepFn(opts.Backoff)
	}
}

// tryAcquireHeavy runs one attempt. It returns (adm,_,nil) on success,
// (nil,true,nil) to back off and retry, or (nil,_,err) to fail closed. The
// watchdog gate runs first: Critical/Warn or a CheckFn error backs off without
// touching a slot (DT-v3-7, fail-closed DT-v3-2).
func tryAcquireHeavy(opts Options) (*Admission, bool, error) {
	dec, err := opts.CheckFn()
	if err != nil {
		// Fail-closed: a watchdog read error never admits. Back off and retry so
		// a transient /proc blip recovers, but never fail-open.
		return nil, true, fmt.Errorf("admit: watchdog check failed (fail-closed): %w", err)
	}
	if dec == memwatchdog.DecisionCritical || dec == memwatchdog.DecisionWarn {
		return nil, true, nil
	}
	slotPath, release, ok, err := grabFreeSlot(opts)
	if err != nil {
		// Non-contention flock error (EACCES/ENOSPC/bad path): fail closed instead
		// of masking it as "busy" and timing out silently (L2).
		return nil, false, err
	}
	if !ok {
		return nil, true, nil
	}
	docker, dockerOK := grabDockerSubSlot(opts)
	if !dockerOK {
		release()
		return nil, true, nil
	}
	// Reserve a deterministic unit name and persist the FULL record (unit + PID +
	// start-ticks) while holding the lock, BEFORE the payload starts. A SIGKILLed
	// admit then always leaves a reapable record — there is no empty-unit window
	// for an orphan to leak through (C1/H1, replaces the old stderr-scrape).
	unit := unitNameFor(slotPath, os.Getpid())
	adm := &Admission{opts: opts, slotPath: slotPath, release: release, docker: docker, unit: unit}
	if err := writeSlotRecord(opts, slotPath, unit); err != nil {
		_ = adm.Release()
		return nil, false, err
	}
	return adm, false, nil
}

// grabFreeSlot tries each heavy slot in fixed order. On the first slot it can
// flock, it reaps an orphaned recorded unit (DT-v3-2) and returns the held slot.
// A genuinely-contended slot (errSlotBusy) is skipped; any OTHER FlockFn error
// (EACCES, ENOSPC, bad path) returns as an error so Acquire fails closed rather
// than silently treating it as "busy" and timing out (L2). ("",nil,false,nil)
// when every slot is busy.
func grabFreeSlot(opts Options) (string, func(), bool, error) {
	for i := 1; i <= opts.MaxHeavy; i++ {
		path := slotPathFor(opts, i)
		release, err := opts.FlockFn(path)
		if err != nil {
			if errors.Is(err, errSlotBusy) {
				continue // genuinely contended: try the next slot
			}
			return "", nil, false, fmt.Errorf("admit: slot %s: %w", path, err)
		}
		reapOrphan(opts, path)
		return path, release, true, nil
	}
	return "", nil, false, nil
}

// grabDockerSubSlot acquires the count=1 docker sub-slot when --exclusive=docker
// (DT-v3-8). It is acquired AFTER a heavy slot, in fixed order. Returns a no-op
// release when not requested. (nil,false) when the sub-slot is busy.
func grabDockerSubSlot(opts Options) (func(), bool) {
	if opts.Exclusive != "docker" {
		return func() {}, true
	}
	release, err := opts.FlockFn(opts.DockerSlotPath)
	if err != nil {
		return nil, false
	}
	return release, true
}

// reapOrphan stops the unit recorded in a just-acquired slot when its admit PID
// is dead (orphaned holder, DT-v3-2). A live recorded PID is never reaped. A
// `systemctl stop` of an already-stopped unit is a harmless no-op.
func reapOrphan(opts Options, slotPath string) {
	rec, ok, err := readSlotRecord(opts, slotPath)
	if err != nil {
		// A genuine read/parse error (not "no record"): surface it instead of
		// silently reusing the slot and masking a possible live orphan (M3).
		slog.Warn("admit_reap_read_error", "event", "admit_reap_read_error", "slot", slotPath, "err", err.Error())
		return
	}
	if !ok || rec.Unit == "" {
		return
	}
	if isHolderAlive(opts, rec) {
		return // recorded holder still the original live process: not an orphan
	}
	stopUnit(opts, rec.Unit)
	// admit_reaped: an orphaned holder's unit was co-terminated before the slot
	// was reused (SPECv3 §observability / DT-v3-2).
	slog.Info("admit_reaped", "event", "admit_reaped", "slot", slotPath, "unit", rec.Unit, "orphan_pid", rec.PID)
}

// isHolderAlive reports whether the recorded holder is still the ORIGINAL live
// process: alive (kill -0) AND its /proc start-ticks still match the record. A
// recycled PID (same number, different start-time) is treated as dead so its
// orphaned unit is reaped — the PID-reuse defense the bare kill -0 lacks (C2,
// mirrors internal/dockerlock). With no recorded start-ticks it falls back to
// liveness only (back-compat with records written before this field existed).
func isHolderAlive(opts Options, rec slotRecord) bool {
	if opts.PidAliveFn == nil || !opts.PidAliveFn(rec.PID) {
		return false
	}
	if opts.PidStartTicksFn == nil || rec.PIDStartTicks == 0 {
		return true
	}
	ticks, err := opts.PidStartTicksFn(rec.PID)
	if err != nil {
		return false // cannot verify identity → treat as orphan and reap (fail-safe)
	}
	return ticks == rec.PIDStartTicks
}

// readSlotRecord returns (record,true,nil) for a present valid record,
// (zero,false,nil) for "no record yet" (absent/empty file), and (zero,false,err)
// for a genuine read or JSON-parse failure — so reapOrphan can distinguish "safe
// to proceed" from "could not verify, don't mask an orphan" (M3).
func readSlotRecord(opts Options, slotPath string) (slotRecord, bool, error) {
	if opts.ReadFileFn == nil {
		return slotRecord{}, false, nil
	}
	data, err := opts.ReadFileFn(slotPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return slotRecord{}, false, nil
		}
		return slotRecord{}, false, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return slotRecord{}, false, nil
	}
	var rec slotRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return slotRecord{}, false, fmt.Errorf("admit: slot record corrompido %s: %w", slotPath, err)
	}
	return rec, true, nil
}

func writeSlotRecord(opts Options, slotPath, unit string) error {
	if opts.WriteFileFn == nil {
		return nil
	}
	var startTicks uint64
	if opts.PidStartTicksFn != nil {
		if t, err := opts.PidStartTicksFn(os.Getpid()); err == nil {
			startTicks = t
		}
	}
	rec := slotRecord{Unit: unit, PID: os.Getpid(), PIDStartTicks: startTicks, AcquiredAt: opts.NowFn().UTC()}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("admit: serializar slot record: %w", err)
	}
	if err := opts.WriteFileFn(slotPath, data, uint32(slotRecordMode)); err != nil {
		return fmt.Errorf("admit: gravar slot record %s: %w", slotPath, err)
	}
	return nil
}

func slotPathFor(opts Options, i int) string {
	return fmt.Sprintf("%s%d.lock", opts.SlotPrefix, i)
}

func validate(opts Options) error {
	if opts.MaxHeavy < 1 {
		return fmt.Errorf("admit: MaxHeavy deve ser >=1, got %d", opts.MaxHeavy)
	}
	if opts.Weight != WeightHeavy && opts.Weight != WeightLight {
		return fmt.Errorf("admit: weight invalido %q (use heavy ou light)", opts.Weight)
	}
	if opts.Weight == WeightLight {
		return nil
	}
	if err := validateAbsPrefix(opts.SlotPrefix, "slot-prefix"); err != nil {
		return err
	}
	if opts.Exclusive == "docker" {
		if err := validateAbsPrefix(opts.DockerSlotPath, "docker-slot-path"); err != nil {
			return err
		}
	}
	return nil
}

func validateAbsPrefix(path, field string) error {
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("admit: %s contem byte NUL", field)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("admit: %s deve ser caminho absoluto, got %q", field, path)
	}
	return nil
}

func applyDefaults(opts *Options) {
	if opts.MaxHeavy == 0 {
		opts.MaxHeavy = civm.DefaultAdmitMaxHeavy
	}
	if opts.SlotPrefix == "" {
		opts.SlotPrefix = civm.DefaultAdmitSlotPathPrefix
	}
	if opts.DockerSlotPath == "" {
		opts.DockerSlotPath = civm.DefaultAdmitDockerSlotPath
	}
	if opts.Weight == "" {
		opts.Weight = WeightHeavy
	}
	if opts.WaitBudget <= 0 {
		opts.WaitBudget = time.Duration(civm.DefaultAdmitWaitMinutes) * time.Minute
	}
	if opts.Backoff <= 0 {
		opts.Backoff = backoffStep
	}
	if opts.NowFn == nil {
		opts.NowFn = time.Now
	}
	if opts.SleepFn == nil {
		opts.SleepFn = time.Sleep
	}
	if opts.CheckFn == nil {
		opts.CheckFn = defaultCheck
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.PidAliveFn == nil {
		opts.PidAliveFn = defaultPidAlive
	}
	if opts.PidStartTicksFn == nil {
		opts.PidStartTicksFn = procstat.PidStartTicks
	}
	if opts.FlockFn == nil {
		opts.FlockFn = defaultFlockNB
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = os.ReadFile
	}
	if opts.WriteFileFn == nil {
		opts.WriteFileFn = defaultWriteFile
	}
}
