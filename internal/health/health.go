// Package health provides read-only system health collection for the
// civm runner host. Designed to be testable: all OS interactions are
// behind small interfaces that tests can fake.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// jsonEncoder retorna encoder com indent (separado para testabilidade).
func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc
}

// Status is the severity of a single check.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusCritical
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusWarn:
		return "WARN"
	case StatusCritical:
		return "CRIT"
	}
	return "?"
}

// Check is one row of the report.
type Check struct {
	Name   string
	Detail string
	Status Status
}

// TimerState is the systemd state of one expected civm timer.
type TimerState struct {
	Enabled string
	Active  string
}

type ServiceState struct {
	LoadState string
	Result    string
}

// Report is the full health report.
type Report struct {
	Checks []Check
}

// Exit returns the worst severity as exit code (0/1/2).
func (r Report) Exit() int {
	worst := StatusOK
	for _, c := range r.Checks {
		if c.Status > worst {
			worst = c.Status
		}
	}
	return int(worst)
}

// Collector knows how to gather health data. Production implementation
// uses real OS calls; tests inject fakes.
type Collector struct {
	WorkDir        string
	DiskWarnFreeGB int64
	DiskCritFreeGB int64
	MemWarnFreeMB  int64
	MemCritFreeMB  int64

	StatfsFn       func(path string) (totalBytes, freeBytes uint64, err error)
	MeminfoFn      func() (memAvailableKB int64, err error)
	RunnerUnitsFn  func(ctx context.Context) ([]string, error)
	LastCleanupFn  func(ctx context.Context) (*time.Time, string, error)
	TimerStateFn   func(ctx context.Context, timer string) (TimerState, error)
	ServiceStateFn func(ctx context.Context, service string) (ServiceState, error)
}

// NewDefaultCollector wires the production implementations.
func NewDefaultCollector(workDir string) *Collector {
	return &Collector{
		WorkDir:        workDir,
		DiskWarnFreeGB: 10,
		DiskCritFreeGB: 3,
		MemWarnFreeMB:  512,
		MemCritFreeMB:  128,
		StatfsFn:       defaultStatfs,
		MeminfoFn:      defaultMeminfo,
		RunnerUnitsFn:  defaultRunnerUnits,
		LastCleanupFn:  defaultLastCleanup,
		TimerStateFn:   defaultTimerState,
		ServiceStateFn: defaultServiceState,
	}
}

// Collect runs every check; never panics, returns Report with checks
// even if some fail (failures become a Check with StatusWarn).
func (c *Collector) Collect(ctx context.Context) Report {
	if c.TimerStateFn == nil {
		c.TimerStateFn = defaultTimerState
	}
	if c.ServiceStateFn == nil {
		c.ServiceStateFn = defaultServiceState
	}
	var r Report
	r.Checks = append(r.Checks, c.checkDisk())
	r.Checks = append(r.Checks, c.checkMem())
	r.Checks = append(r.Checks, c.checkRunners(ctx))
	r.Checks = append(r.Checks, c.checkTimer(ctx, "TIMER_CLEANUP", "civmctl-cleanup.timer", StatusCritical))
	r.Checks = append(r.Checks, c.checkTimer(ctx, "TIMER_DISK", "civmctl-disk-watchdog.timer", StatusCritical))
	r.Checks = append(r.Checks, c.checkTimer(ctx, "TIMER_RUNNER", "civmctl-runner-watchdog.timer", StatusWarn))
	r.Checks = append(r.Checks, c.checkTimer(ctx, "TIMER_REVERSE", "civmctl-reverse-watchdog.timer", StatusWarn))
	r.Checks = append(r.Checks, c.checkTimer(ctx, "TIMER_METRICS", "civmctl-metrics.timer", StatusWarn))
	r.Checks = append(r.Checks, c.checkTimer(ctx, "TIMER_REAPER", "civmctl-run-reaper.timer", StatusCritical))
	r.Checks = append(
		r.Checks,
		c.checkServiceResult(
			ctx,
			"SERVICE_RUNNER_WATCHDOG",
			"civmctl-runner-watchdog.service",
			StatusCritical,
		),
	)
	r.Checks = append(r.Checks, c.checkServiceResult(ctx, "SERVICE_REAPER", "civmctl-run-reaper.service", StatusCritical))
	r.Checks = append(r.Checks, c.checkLastCleanup(ctx))
	return r
}

func (c *Collector) checkServiceResult(ctx context.Context, name, service string, failedStatus Status) Check {
	state, err := c.ServiceStateFn(ctx, service)
	if err != nil {
		return Check{Name: name, Detail: fmt.Sprintf("%s result unavailable: %v", service, err), Status: StatusWarn}
	}
	if state.LoadState != "loaded" {
		return Check{Name: name, Detail: fmt.Sprintf("%s missing: load_state=%s", service, state.LoadState), Status: failedStatus}
	}
	if state.Result != "success" {
		return Check{
			Name:   name,
			Detail: fmt.Sprintf("%s failed: result=%s", service, state.Result),
			Status: failedStatus,
		}
	}
	return Check{Name: name, Detail: service + " result=success", Status: StatusOK}
}

func (c *Collector) checkDisk() Check {
	total, free, err := c.StatfsFn(c.WorkDir)
	if err != nil {
		return Check{Name: "DISK", Detail: c.WorkDir + ": " + err.Error(), Status: StatusWarn}
	}
	freeGB := int64(free / (1 << 30))
	totalGB := int64(total / (1 << 30))
	st := StatusOK
	if freeGB < c.DiskCritFreeGB {
		st = StatusCritical
	} else if freeGB < c.DiskWarnFreeGB {
		st = StatusWarn
	}
	return Check{
		Name:   "DISK",
		Detail: fmt.Sprintf("%s %d GB free / %d GB", c.WorkDir, freeGB, totalGB),
		Status: st,
	}
}

func (c *Collector) checkMem() Check {
	availKB, err := c.MeminfoFn()
	if err != nil {
		return Check{Name: "MEM", Detail: err.Error(), Status: StatusWarn}
	}
	availMB := availKB / 1024
	st := StatusOK
	if availMB < c.MemCritFreeMB {
		st = StatusCritical
	} else if availMB < c.MemWarnFreeMB {
		st = StatusWarn
	}
	return Check{
		Name:   "MEM",
		Detail: fmt.Sprintf("%d MB available", availMB),
		Status: st,
	}
}

func (c *Collector) checkRunners(ctx context.Context) Check {
	units, err := c.RunnerUnitsFn(ctx)
	if err != nil {
		return Check{Name: "RUNNERS", Detail: err.Error(), Status: StatusWarn}
	}
	if len(units) == 0 {
		return Check{Name: "RUNNERS", Detail: "nenhum runner ativo (esperado fora da VM)", Status: StatusOK}
	}
	return Check{Name: "RUNNERS", Detail: strings.Join(units, ", "), Status: StatusOK}
}

func (c *Collector) checkTimer(ctx context.Context, name, timer string, missingStatus Status) Check {
	state, err := c.TimerStateFn(ctx, timer)
	if err != nil {
		return Check{Name: name, Detail: fmt.Sprintf("%s missing: %v", timer, err), Status: missingStatus}
	}
	if state.Enabled != "enabled" || state.Active != "active" {
		return Check{
			Name:   name,
			Detail: fmt.Sprintf("%s stale: enabled=%s active=%s", timer, state.Enabled, state.Active),
			Status: missingStatus,
		}
	}
	return Check{Name: name, Detail: timer + " enabled+active", Status: StatusOK}
}

func (c *Collector) checkLastCleanup(ctx context.Context) Check {
	when, recovered, err := c.LastCleanupFn(ctx)
	if err != nil {
		return Check{Name: "LAST", Detail: err.Error(), Status: StatusWarn}
	}
	if when == nil {
		return Check{Name: "LAST", Detail: "cleanup timer nunca rodou (pode nao ter disparado ainda)", Status: StatusWarn}
	}
	age := time.Since(*when)
	st := StatusOK
	if age > 48*time.Hour {
		st = StatusWarn
	}
	return Check{
		Name:   "LAST",
		Detail: fmt.Sprintf("cleanup %s atras, %s", roundDur(age), recovered),
		Status: st,
	}
}

func roundDur(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// RenderJSON writes the report as machine-readable JSON.
// Útil para integrações com Prometheus/dashboards que parseiam stdout.
func (r Report) RenderJSON(w io.Writer) error {
	type checkOut struct {
		Name   string `json:"name"`
		Detail string `json:"detail"`
		Status string `json:"status"`
	}
	type out struct {
		Checks []checkOut `json:"checks"`
		Exit   int        `json:"exit"`
	}
	o := out{Exit: r.Exit()}
	for _, c := range r.Checks {
		o.Checks = append(o.Checks, checkOut{
			Name:   c.Name,
			Detail: c.Detail,
			Status: c.Status.String(),
		})
	}
	enc := jsonEncoder(w)
	return enc.Encode(o)
}

// Render writes a fixed-width table.
func (r Report) Render(w io.Writer) {
	fmt.Fprintf(w, "%-8s %-50s %s\n", "CHECK", "DETALHE", "STATUS")
	fmt.Fprintln(w, strings.Repeat("-", 70))
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-8s %-50s %s\n", c.Name, truncate(c.Detail, 50), c.Status)
	}
	fmt.Fprintf(w, "EXIT: %d\n", r.Exit())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ---- default OS implementations (not exercised by unit tests) ----

func defaultStatfs(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total := uint64(st.Blocks) * uint64(st.Bsize)
	free := uint64(st.Bavail) * uint64(st.Bsize)
	return total, free, nil
}

func defaultMeminfo() (int64, error) {
	out, err := readFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMemAvailableKB(string(out))
}

// parseMemAvailableKB extracts MemAvailable from /proc/meminfo content.
func parseMemAvailableKB(s string) (int64, error) {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("malformed line: %q", line)
			}
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("MemAvailable nao encontrado em /proc/meminfo")
}

func defaultRunnerUnits(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "list-units", "--type=service", "--no-pager", "--no-legend", "actions.runner.*")
	out, err := cmd.Output()
	if err != nil {
		// systemctl may return non-zero if no units match; treat as empty.
		return nil, nil
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			units = append(units, fields[0])
		}
	}
	return units, nil
}

var commandOutput = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func defaultLastCleanup(ctx context.Context) (*time.Time, string, error) {
	out, err := commandOutput(ctx, "journalctl", "-u", "civmctl-cleanup.service", "--since", "7 days ago", "--no-pager", "--reverse", "-n", "1", "-o", "short-iso")
	if err != nil {
		return nil, "", nil
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil, "", nil
	}
	// short-iso prefix: 2026-05-10T03:42:11+0000 ...
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 0 {
		return nil, "", nil
	}
	t, ok := parseJournalShortISO(parts[0])
	if !ok {
		return nil, "", nil
	}
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	return &t, rest, nil
}

func parseJournalShortISO(value string) (time.Time, bool) {
	for _, layout := range []string{
		time.RFC3339,               // 2026-05-21T04:03:59+00:00
		"2006-01-02T15:04:05-0700", // 2026-05-21T04:03:59+0000
	} {
		t, err := time.Parse(layout, value)
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func defaultTimerState(ctx context.Context, timer string) (TimerState, error) {
	enabledOut, enabledErr := exec.CommandContext(ctx, "systemctl", "is-enabled", timer).Output()
	activeOut, activeErr := exec.CommandContext(ctx, "systemctl", "is-active", timer).Output()
	state := TimerState{
		Enabled: strings.TrimSpace(string(enabledOut)),
		Active:  strings.TrimSpace(string(activeOut)),
	}
	if enabledErr != nil {
		return state, fmt.Errorf("is-enabled: %w", enabledErr)
	}
	if activeErr != nil {
		return state, fmt.Errorf("is-active: %w", activeErr)
	}
	return state, nil
}

func defaultServiceState(ctx context.Context, service string) (ServiceState, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", service, "--property=LoadState,Result").Output()
	if err != nil {
		return ServiceState{}, fmt.Errorf("systemctl show LoadState,Result: %w", err)
	}
	state := ServiceState{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "LoadState":
			state.LoadState = value
		case "Result":
			state.Result = value
		}
	}
	if state.LoadState == "" || state.Result == "" {
		return state, fmt.Errorf("systemctl show returned incomplete state")
	}
	return state, nil
}

// readFile is a tiny wrapper to make defaultMeminfo testable via build tags
// in the future. Today it's a passthrough.
func readFile(path string) ([]byte, error) {
	return osReadFile(path)
}
