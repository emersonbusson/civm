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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emersonbusson/civm/internal/admit"
	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/memwatchdog"
)

// Exit codes for `civmctl admit` (SPECv3 DT-v3-3, analogous to the lock codes).
// Distinct from 64 (usage), 75/77 (lock) so a caller/job-timeout can branch.
const (
	exitAdmitWaitTimeout = 78 // ErrWaitBudgetExceeded: no heavy slot within WaitBudget
	exitAdmitInternal    = 79 // admit/flock/systemd-run failure in the admit layer
)

// admitDefaultUser is the last-resort runner user when sudo/current-user lookup
// is unavailable. SPECv3 DT-v3-1: the payload MUST run as the runner user, never
// root.
const admitDefaultUser = "emdev"

// admitEvent names for the structured stderr log (SPECv3 §observability).
const (
	admitEventAcquired    = "admit_acquired"
	admitEventWait        = "admit_wait"
	admitEventWaitTimeout = "admit_wait_timeout"
	admitEventReleased    = "admit_released"
)

// admitFlags is the parsed CLI surface.
type admitFlags struct {
	weight    string
	exclusive string
	waitMin   int
	jsonOut   bool
}

// systemdRunSpec is the input to buildSystemdRunArgs (DT-v3-1).
type systemdRunSpec struct {
	User  string
	Group string
	MemMB int64
	Unit  string // deterministic --unit name; "" lets systemd-run auto-name (light)
	Cmd   []string
}

// runAdmit gates a memory-heavy CI payload: it acquires an admission slot, then
// runs the payload wrapped in a transient systemd service (`sudo systemd-run
// --pipe --wait -p User=<runner> -p MemoryMax=<eff>M -p MemorySwapMax=0`) so the
// job runs as the runner user under a cgroup memory cap (DT-v3-1). The unit name
// is captured and recorded for co-termination (DT-v3-2). Light jobs run with no
// slot. The subcommand is inert until a workflow calls it (forward-only).
func runAdmit(args []string) int {
	var af admitFlags
	fs := flag.NewFlagSet("admit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&af.weight, "weight", "auto", "heavy|light|auto (auto: CIVM_JOB_WEIGHT ou heavy)")
	fs.StringVar(&af.exclusive, "exclusive", "", "recurso exclusivo serializado (ex: docker)")
	fs.IntVar(&af.waitMin, "wait-minutes", civm.DefaultAdmitWaitMinutes, "WaitBudget em minutos antes do exit tipado")
	fs.BoolVar(&af.jsonOut, "json", false, "emitir eventos de admissao em JSON no stderr")
	fs.Bool("exec", false, "executar o comando apos -- sob a admissao")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de admit:", err)
		return exitUsage
	}
	payload := fs.Args()
	if len(payload) == 0 {
		fmt.Fprintln(os.Stderr, "uso: civmctl admit [--weight heavy|light|auto] [--exclusive docker] [--wait-minutes 30] --exec -- <cmd...>")
		return exitUsage
	}
	weight, err := resolveWeight(af.weight, os.Getenv("CIVM_JOB_WEIGHT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de admit:", err)
		return exitUsage
	}
	return admitAndRun(af, weight, payload)
}

// admitAndRun acquires the admission and runs the wrapped payload. Split from
// runAdmit so the flag/usage layer stays small and testable.
func admitAndRun(af admitFlags, weight admit.Weight, payload []string) int {
	opts := admit.Options{
		MaxHeavy:       civm.DefaultAdmitMaxHeavy,
		HeavyMaxMB:     civm.DefaultAdmitHeavyMaxMB,
		SlotPrefix:     civm.DefaultAdmitSlotPathPrefix,
		DockerSlotPath: civm.DefaultAdmitDockerSlotPath,
		Weight:         weight,
		Exclusive:      af.exclusive,
		WaitBudget:     time.Duration(af.waitMin) * time.Minute,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if weight == admit.WeightHeavy {
		// Provision the heavy-slot runtime dir (/run/civm) up front: /run is
		// root-owned so the flock seam cannot mkdir it. Without this, every slot
		// flock fails and Acquire silently times out (exit 78) — fail loud instead.
		if err := ensureAdmitRunDir(filepath.Dir(civm.DefaultAdmitSlotPathPrefix)); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao provisionar dir de slots:", err)
			return exitAdmitInternal
		}
		// DT-v3-6 (revised): without an enforceable cgroup memory controller the
		// MemoryMax cap is a no-op, so a "heavy" slot would gate count but not RAM —
		// the blind-counting mode the spec forbids. Fail closed instead of pretending.
		if !cgroupMemoryEnforceable() {
			fmt.Fprintln(os.Stderr, "cgroup v2 memory controller ausente; recusando heavy (fail-closed, DT-v3-6)")
			return exitAdmitInternal
		}
		emitAdmitEvent(af.jsonOut, admitEvent{Event: admitEventWait, Weight: string(weight), WaitMin: af.waitMin})
	}
	adm, err := admit.Acquire(ctx, opts)
	if err != nil {
		if errors.Is(err, admit.ErrWaitBudgetExceeded) {
			emitAdmitEvent(af.jsonOut, admitEvent{Event: admitEventWaitTimeout, Weight: string(weight), WaitMin: af.waitMin})
			return exitAdmitWaitTimeout
		}
		fmt.Fprintln(os.Stderr, "erro ao adquirir admissao:", err)
		return exitAdmitInternal
	}
	emitAdmitEvent(af.jsonOut, admitEvent{Event: admitEventAcquired, Weight: string(weight), Slot: adm.SlotPath(), PID: os.Getpid()})

	released := make(chan struct{})
	releaseOnce := admitReleaseFunc(adm, af, released)
	defer releaseOnce()
	return runUnderAdmission(ctx, adm, releaseOnce, released, payload)
}

// runUnderAdmission builds the systemd-run wrapper, traps signals (Release +
// forward to the child), runs the payload with inherited stdio (--pipe) and
// propagates the child's exit code.
func runUnderAdmission(ctx context.Context, adm *admit.Admission, releaseOnce func(), released chan struct{}, payload []string) int {
	memMB, err := effectiveMemMB(civm.DefaultAdmitHeavyMaxMB, hostMemTotalMB(), civm.DefaultAdmitHostReserveMB, civm.DefaultAdmitMaxHeavy)
	if err != nil {
		// Fail closed: never admit with an unbounded/overcommitting cap (H3).
		fmt.Fprintln(os.Stderr, "erro no MemoryMax efetivo:", err)
		return exitAdmitInternal
	}
	// The unit name was reserved deterministically by Acquire and already written
	// into the slot record, so --unit makes systemd-run create exactly that unit:
	// reap-on-reuse can co-terminate it even if admit is SIGKILLed before the
	// child prints anything (DT-v3-2). No stderr scraping, no race.
	argv := buildSystemdRunArgs(systemdRunSpec{
		User:  runnerUser(),
		Group: runnerUser(),
		MemMB: memMB,
		Unit:  adm.UnitName(),
		Cmd:   payload,
	})
	//nolint:gosec // G204: argv[0] is the fixed "sudo systemd-run" wrapper; the
	// payload after -- comes from the workflow's own job step, same trust model
	// as `civmctl lock --exec`.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go forwardSignal(sigCh, cmd, releaseOnce, released)

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao iniciar systemd-run:", err)
		return exitAdmitInternal
	}
	code := waitChild(cmd)
	releaseOnce()
	return code
}

// waitChild waits for the wrapped command and returns its exit code, propagating
// a non-zero child exit (SPECv3: propagate the child's exit code).
func waitChild(cmd *exec.Cmd) int {
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "erro ao executar payload:", err)
		return exitAdmitInternal
	}
	return 0
}

// forwardSignal co-terminates on a trapped signal: it releases the admission
// (which stops the recorded unit and frees the slot) and forwards the signal to
// systemd-run so --wait observes the unit's exit and waitChild returns. It does
// NOT cancel the context here: CommandContext's cancel sends SIGKILL, which would
// race the graceful stop out from under it (H2). The deferred cancel() in
// admitAndRun remains the last-resort cleanup if the process is still alive.
func forwardSignal(sigCh <-chan os.Signal, cmd *exec.Cmd, releaseOnce func(), released <-chan struct{}) {
	select {
	case sig := <-sigCh:
		releaseOnce()
		if cmd.Process != nil {
			_ = cmd.Process.Signal(sig)
		}
	case <-released:
	}
}

// admitReleaseFunc returns an idempotent release that stops the unit, frees the
// slot and emits the admit_released event exactly once. The guard is a sync.Once
// because it is called from BOTH the main flow and the forwardSignal goroutine:
// a plain bool would be a data race and could double-close(released) → panic (M1).
func admitReleaseFunc(adm *admit.Admission, af admitFlags, released chan struct{}) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := adm.Release(); err != nil {
				fmt.Fprintln(os.Stderr, "erro ao liberar admissao:", err)
			}
			emitAdmitEvent(af.jsonOut, admitEvent{Event: admitEventReleased, Slot: adm.SlotPath(), PID: os.Getpid()})
			close(released)
		})
	}
}

// resolveWeight maps the --weight flag (and CIVM_JOB_WEIGHT for auto) to a
// Weight. auto → env value if it is heavy/light, else heavy (SPECv3 ITEM-4).
func resolveWeight(flagVal, envVal string) (admit.Weight, error) {
	switch admit.Weight(strings.TrimSpace(flagVal)) {
	case admit.WeightHeavy:
		return admit.WeightHeavy, nil
	case admit.WeightLight:
		return admit.WeightLight, nil
	case "auto":
		switch admit.Weight(strings.TrimSpace(envVal)) {
		case admit.WeightLight:
			return admit.WeightLight, nil
		case admit.WeightHeavy:
			return admit.WeightHeavy, nil
		default:
			return admit.WeightHeavy, nil
		}
	default:
		return "", fmt.Errorf("weight invalido %q (use heavy, light ou auto)", flagVal)
	}
}

// effectiveMemMB computes the per-job cgroup MemoryMax in MB (DT-v3-5). A
// calibrated HeavyMaxMB>0 wins verbatim; otherwise it is generous:
// (MemTotal-host)/MaxHeavy — a value that, being the integer quotient, inherently
// satisfies the invariant eff×MaxHeavy ≤ MemTotal-host. It FAILS CLOSED (error,
// no admission) when the host total is unreadable (0) or so small that even the
// floor would overcommit MaxHeavy concurrent jobs — admitting a generous cap on a
// box we couldn't measure, or overcommitting a tiny one, is the H3 fail-open.
func effectiveMemMB(heavyMaxMB, memTotalMB, hostReserveMB int64, maxHeavy int) (int64, error) {
	if heavyMaxMB > 0 {
		return heavyMaxMB, nil
	}
	if maxHeavy < 1 {
		maxHeavy = 1
	}
	if memTotalMB <= 0 {
		return 0, fmt.Errorf("MemTotal do host indisponivel; recusando heavy (fail-closed)")
	}
	budget := (memTotalMB - hostReserveMB) / int64(maxHeavy)
	if budget < admitMinMemMB {
		return 0, fmt.Errorf("host pequeno demais para %d heavy: (MemTotal %dMB - reserva %dMB)/%d < piso %dMB",
			maxHeavy, memTotalMB, hostReserveMB, maxHeavy, admitMinMemMB)
	}
	return budget, nil
}

// admitMinMemMB is the floor for a generous MemoryMax so a tiny/misreported host
// never yields a zero/negative cgroup cap.
const admitMinMemMB int64 = 512

// buildSystemdRunArgs renders the DT-v3-1 wrapper argv: a transient systemd
// SERVICE (--pipe --wait) — never --scope (which would run as root). --collect
// garbage-collects the transient unit when it finishes (even on a non-zero exit)
// so completed jobs never linger as "failed" units. The payload follows the --
// separator unchanged.
func buildSystemdRunArgs(spec systemdRunSpec) []string {
	prefix := []string{"sudo", "systemd-run", "--collect"}
	if spec.Unit != "" {
		// Pre-assigned unit name (heavy): systemd-run creates exactly this unit so
		// the slot record (written before start) can co-terminate it on reap.
		prefix = append(prefix, "--unit="+spec.Unit)
	}
	prefix = append(prefix,
		"--pipe", "--wait",
		"-p", "User="+spec.User,
		"-p", "Group="+spec.Group,
		"-p", fmt.Sprintf("MemoryMax=%dM", spec.MemMB),
		"-p", "MemorySwapMax=0",
		"--",
	)
	args := make([]string, 0, len(prefix)+len(spec.Cmd))
	args = append(args, prefix...)
	return append(args, spec.Cmd...)
}

// runnerUser returns the runner user for the cgroup service: SUDO_USER when
// `admit` was invoked under sudo, else the current process user (DT-v3-1).
func runnerUser() string {
	return civm.ResolveRunnerUser(admitDefaultUser)
}

// ensureAdmitRunDir provisions the heavy-slot runtime dir (e.g. /run/civm) so the
// flock slots can be created. /run is root-owned, so the runner cannot mkdir
// there; `sudo install -d` creates it once, owned by the runner user (the runner
// has NOPASSWD sudo, the same trust model as the systemd-run wrapper). It is
// idempotent and a no-op when a tmpfiles.d entry already provisioned the dir.
// Returns an error only when the dir is genuinely absent and cannot be created.
func ensureAdmitRunDir(dir string) error {
	user := runnerUser()
	//nolint:gosec // fixed verb; dir is the compiled-in slot prefix's parent.
	out, err := exec.Command("sudo", "install", "-d", "-o", user, "-g", user, "-m", "0755", dir).CombinedOutput()
	if err == nil {
		return nil
	}
	// Fallback: a tmpfiles.d entry may already own the dir even when install fails
	// (e.g. restricted sudo). Accept it only if it is a real directory the runner
	// can WRITE — a root-owned 0700 dir would pass IsDir yet fail every slot open
	// with EACCES, which Acquire would surface as a confusing silent timeout (L1).
	if fi, statErr := os.Stat(dir); statErr == nil && fi.IsDir() {
		if syscall.Access(dir, 0x2 /* W_OK */) == nil {
			return nil
		}
	}
	return fmt.Errorf("%s: %w (%s)", dir, err, strings.TrimSpace(string(out)))
}

// cgroupMemoryEnforceable reports whether cgroup v2's memory controller is
// available — i.e. whether `systemd-run -p MemoryMax` can actually enforce a cap.
// When it is not, admit refuses heavy jobs (DT-v3-6, revised): a count-limiter
// with no RAM bound is exactly the blind-counting the spec forbids. The doctor's
// ADMIT_CGROUP check reports the same condition ahead of time. (Read directly;
// the testable form of this detection lives in internal/doctor.)
func cgroupMemoryEnforceable() bool {
	data, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return false
	}
	for _, c := range strings.Fields(string(data)) {
		if c == "memory" {
			return true
		}
	}
	return false
}

// hostMemTotalMB reads MemTotal best-effort for the generous MemoryMax. On a
// read failure it returns 0, which effectiveMemMB treats as fail-closed (H3).
func hostMemTotalMB() int64 {
	mem, err := memwatchdog.Sample(memwatchdog.DefaultOptions())
	if err != nil {
		return 0
	}
	return mem.MemTotalMB
}

// admitEvent is the JSON shape emitted to stderr (best-effort observability;
// never alters the exit code).
type admitEvent struct {
	Timestamp string `json:"timestamp"`
	Event     string `json:"event"`
	Weight    string `json:"weight,omitempty"`
	Slot      string `json:"slot,omitempty"`
	WaitMin   int    `json:"wait_minutes,omitempty"`
	PID       int    `json:"pid,omitempty"`
}

func emitAdmitEvent(jsonOut bool, ev admitEvent) {
	ev.Timestamp = time.Now().UTC().Format(time.RFC3339)
	if jsonOut {
		data, err := json.Marshal(ev)
		if err != nil {
			return
		}
		fmt.Fprintln(os.Stderr, string(data))
		return
	}
	fmt.Fprintf(os.Stderr, "admit %s weight=%s slot=%s pid=%d\n", ev.Event, ev.Weight, ev.Slot, ev.PID)
}
