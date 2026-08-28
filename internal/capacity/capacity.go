// Package capacity exposes a stable status contract for integrations such as Busson.
package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/dockerlock"
)

type Report struct {
	DiskPath       string `json:"disk_path"`
	DiskUsedPct    int    `json:"disk_used_pct"`
	DiskFreeGB     int64  `json:"disk_free_gb"`
	DiskTotalGB    int64  `json:"disk_total_gb"`
	RunnerServices int    `json:"runner_services"`
	RunnerWorkers  int    `json:"runner_workers"`
	AcceptingJobs  bool   `json:"accepting_jobs"`
	Reason         string `json:"reason,omitempty"`
	QueueStalled   bool   `json:"queue_stalled"`
	QueueStallUnit string `json:"queue_stall_unit,omitempty"`
	// DockerHeavyLockActive mirrors dockerlock.IsActive: a docker-heavy job is
	// holding the box-wide serialization lock right now (ITEM-7 / DT-v2-13).
	DockerHeavyLockActive bool `json:"docker_heavy_lock_active"`
	// DockerHeavyLockHolder is a non-PII label of the current holder (scope and
	// repo/run when available), empty when no fresh lock is held.
	DockerHeavyLockHolder string `json:"docker_heavy_lock_holder,omitempty"`
	// RunnerPortBlocks is the best-effort slot->base map read from the port
	// allocation state file; nil when unreadable or empty.
	RunnerPortBlocks map[string]int `json:"runner_port_blocks,omitempty"`
	// Admit reports the memory-admission slot occupancy (heavy_live/max_heavy)
	// read-only by probing the flock slots (SPECv3 ITEM-7 wiring). nil when the
	// admit status could not be sampled.
	Admit *AdmitStatus `json:"admit,omitempty"`
}

// AdmitStatus is the read-only memory-admission view: how many of the MaxHeavy
// flock slots are currently live (held by a heavy job). It is observability
// only — it never blocks job acceptance.
type AdmitStatus struct {
	HeavyLive int `json:"heavy_live"`
	MaxHeavy  int `json:"max_heavy"`
}

type Options struct {
	Path       string
	MaxDiskPct int
	StatfsFn   func(path string) (totalBytes, freeBytes uint64, err error)
	RunFn      func(ctx context.Context, name string, args ...string) ([]byte, error)
	// LockActiveFn reports the docker-heavy lock state (active, holder, err). It
	// is injected so unit tests never touch /run/civm; the default wraps
	// dockerlock.IsActive + dockerlock.Holder.
	LockActiveFn func() (active bool, holder string, err error)
	// PortBlocksFn returns the slot->base allocation map best-effort; the default
	// reads civm.DefaultPortBlockStatePath and returns nil on any error.
	PortBlocksFn func() map[string]int
	// AdmitStatusFn returns the read-only admit slot occupancy; the default
	// probes the flock slots non-destructively. Injected so tests never touch
	// /run/civm.
	AdmitStatusFn func() AdmitStatus
	// QueueStallFn reads the runner watchdog's semantic marker. Parse errors
	// fail closed because advertising capacity while a session is stuck leaves
	// the eligible queue idle despite healthy disk.
	QueueStallFn func() (stalled bool, unit string, err error)
}

func DefaultOptions() Options {
	return Options{
		Path:          "/",
		MaxDiskPct:    civm.DefaultCapacityMaxDiskPct,
		StatfsFn:      defaultStatfs,
		RunFn:         defaultRun,
		LockActiveFn:  defaultLockActive,
		PortBlocksFn:  defaultPortBlocks,
		AdmitStatusFn: defaultAdmitStatus,
		QueueStallFn:  defaultQueueStall,
	}
}

func defaultQueueStall() (bool, string, error) {
	return queueStallFromFile(civm.DefaultRunnerWatchdogMarkerPath)
}

func queueStallFromFile(path string) (bool, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return false, "", nil
	}
	var state struct {
		QueueStalls map[string]struct {
			Signature string `json:"signature"`
		} `json:"queue_stalls"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return false, "", fmt.Errorf("parse runner watchdog marker: %w", err)
	}
	var units []string
	for unit, incident := range state.QueueStalls {
		if strings.TrimSpace(incident.Signature) != "" {
			units = append(units, unit)
		}
	}
	if len(units) == 0 {
		return false, "", nil
	}
	sort.Strings(units)
	return true, units[0], nil
}

// defaultLockActive wraps dockerlock so capacity does not duplicate the
// staleness/PID-reuse logic. Import direction is capacity -> dockerlock only;
// dockerlock never imports capacity (DT-v2-13).
func defaultLockActive() (bool, string, error) {
	opts := dockerlock.DefaultOptions()
	active, err := dockerlock.IsActive(opts)
	if err != nil || !active {
		return false, "", err
	}
	return true, dockerlock.Holder(opts), nil
}

// defaultPortBlocks reads the sticky slot->base map persisted by portblock.
// Any error (missing file, malformed JSON) yields nil — this is observability,
// never a hard dependency.
func defaultPortBlocks() map[string]int {
	data, err := os.ReadFile(civm.DefaultPortBlockStatePath)
	if err != nil {
		return nil
	}
	var allocs []struct {
		Slot string `json:"slot"`
		Base int    `json:"base"`
	}
	if err := json.Unmarshal(data, &allocs); err != nil {
		return nil
	}
	if len(allocs) == 0 {
		return nil
	}
	m := make(map[string]int, len(allocs))
	for _, a := range allocs {
		m[a.Slot] = a.Base
	}
	return m
}

// defaultAdmitStatus probes the memory-admission flock slots read-only and
// returns how many are live (held by a heavy job). It never mutates state and
// never blocks: a free slot is taken with flock(LOCK_NB) and immediately
// released; a busy slot reports live (SPECv3 ITEM-7 wiring, DT-v3-3 no minting).
func defaultAdmitStatus() AdmitStatus {
	maxHeavy := civm.DefaultAdmitMaxHeavy
	return AdmitStatus{
		HeavyLive: countLiveHeavySlots(civm.DefaultAdmitSlotPathPrefix, maxHeavy, probeSlotFree),
		MaxHeavy:  maxHeavy,
	}
}

// countLiveHeavySlots counts slots "{prefix}{1..maxHeavy}.lock" that are NOT
// free. freeFn(path) reports (free, err); a probe error counts the slot as
// not-live (best-effort observability, never a hard dependency).
func countLiveHeavySlots(prefix string, maxHeavy int, freeFn func(path string) (bool, error)) int {
	live := 0
	for i := 1; i <= maxHeavy; i++ {
		path := fmt.Sprintf("%s%d.lock", prefix, i)
		free, err := freeFn(path)
		if err != nil {
			continue
		}
		if !free {
			live++
		}
	}
	return live
}

// probeSlotFree reports whether a heavy slot is free by attempting a
// non-blocking advisory lock and releasing it immediately. A missing slot file
// is free (no holder has ever taken it). EWOULDBLOCK means a live holder.
func probeSlotFree(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil // never claimed → free
		}
		return false, fmt.Errorf("capacity: abrir slot %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil // held by a live heavy job
		}
		return false, fmt.Errorf("capacity: flock probe %s: %w", path, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true, nil
}

func Check(ctx context.Context, opts Options) Report {
	if opts.Path == "" {
		opts.Path = "/"
	}
	if opts.MaxDiskPct == 0 {
		opts.MaxDiskPct = civm.DefaultCapacityMaxDiskPct
	}
	if opts.StatfsFn == nil {
		opts.StatfsFn = defaultStatfs
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.LockActiveFn == nil {
		opts.LockActiveFn = defaultLockActive
	}
	if opts.PortBlocksFn == nil {
		opts.PortBlocksFn = defaultPortBlocks
	}
	if opts.AdmitStatusFn == nil {
		opts.AdmitStatusFn = defaultAdmitStatus
	}
	if opts.QueueStallFn == nil {
		opts.QueueStallFn = defaultQueueStall
	}
	r := Report{DiskPath: opts.Path, AcceptingJobs: true}
	// Lock/port/admit telemetry is best-effort and never blocks job acceptance: a
	// read error leaves DockerHeavyLockActive=false (DT-v2-13).
	active, holder, _ := opts.LockActiveFn()
	r.DockerHeavyLockActive = active
	r.DockerHeavyLockHolder = holder
	r.RunnerPortBlocks = opts.PortBlocksFn()
	admitStatus := opts.AdmitStatusFn()
	r.Admit = &admitStatus
	total, free, err := opts.StatfsFn(opts.Path)
	if err != nil || total == 0 {
		r.AcceptingJobs = false
		r.Reason = fmt.Sprintf("disk stat failed: %v", err)
		return r
	}
	used := total - free
	r.DiskUsedPct = int(used * 100 / total)
	r.DiskFreeGB = int64(free / (1 << 30))
	r.DiskTotalGB = int64(total / (1 << 30))
	r.RunnerServices = countRunnerServices(ctx, opts.RunFn)
	r.RunnerWorkers = countRunnerWorkers(ctx, opts.RunFn)
	stalled, unit, queueErr := opts.QueueStallFn()
	if queueErr != nil {
		r.AcceptingJobs = false
		r.Reason = "runner queue state unavailable: " + queueErr.Error()
	} else if stalled {
		r.QueueStalled = true
		r.QueueStallUnit = unit
		r.AcceptingJobs = false
		r.Reason = "runner queue stall: " + unit
	}
	if r.DiskUsedPct >= opts.MaxDiskPct {
		r.AcceptingJobs = false
		r.Reason = fmt.Sprintf("disk usage %d%% >= %d%%", r.DiskUsedPct, opts.MaxDiskPct)
	}
	return r
}

func RenderJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func RenderText(w io.Writer, r Report) {
	state := "accepting"
	if !r.AcceptingJobs {
		state = "blocked"
	}
	fmt.Fprintf(w, "Capacity: %s, disk=%d%% free=%dGB/%dGB runners=%d workers=%d\n", state, r.DiskUsedPct, r.DiskFreeGB, r.DiskTotalGB, r.RunnerServices, r.RunnerWorkers)
	if r.DockerHeavyLockActive {
		holder := r.DockerHeavyLockHolder
		if holder == "" {
			holder = "(unknown)"
		}
		fmt.Fprintf(w, "Docker-heavy lock: held by %s\n", holder)
	}
	if r.Reason != "" {
		fmt.Fprintf(w, "Reason: %s\n", r.Reason)
	}
}

func countRunnerServices(ctx context.Context, runFn func(context.Context, string, ...string) ([]byte, error)) int {
	out, err := runFn(ctx, "systemctl", "list-units", "--type=service", "--no-pager", "actions.runner.*")
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "actions.runner") {
			count++
		}
	}
	return count
}

func countRunnerWorkers(ctx context.Context, runFn func(context.Context, string, ...string) ([]byte, error)) int {
	out, err := runFn(ctx, "pgrep", "-fc", "Runner.Worker")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func defaultStatfs(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize), nil
}
