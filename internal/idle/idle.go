// Package idle detects whether a civm runner host is safe for mutating
// maintenance operations. It is intentionally read-only: it inspects the
// process table and returns evidence, never kills or pauses work.
package idle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Status is the host-idleness classification.
type Status string

const (
	StatusIdle    Status = "idle"
	StatusBusy    Status = "busy"
	StatusUnknown Status = "unknown"
)

// ExitCode maps Status to the idle-check public contract.
func (s Status) ExitCode() int {
	switch s {
	case StatusIdle:
		return 0
	case StatusBusy:
		return 1
	default:
		return 2
	}
}

// Activity is evidence that a CI job or build is currently active on the host.
type Activity struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
}

// Result is the complete read-only idle check outcome.
type Result struct {
	Status     Status     `json:"status"`
	ExitCode   int        `json:"exit_code"`
	Activities []Activity `json:"activities,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Options controls the process probe. ProbeDelay enables a second probe to
// avoid racing a just-started job between preflight and mutation.
type Options struct {
	ProbeDelay time.Duration
	ActivityFn func(ctx context.Context) ([]Activity, error)
}

// DefaultOptions returns production defaults.
func DefaultOptions() Options {
	return Options{
		ProbeDelay: 2 * time.Second,
		ActivityFn: DefaultActivities,
	}
}

// Check classifies the host as idle, busy or unknown. It is read-only.
func Check(ctx context.Context, opts Options) Result {
	if opts.ActivityFn == nil {
		opts.ActivityFn = DefaultActivities
	}
	first, err := opts.ActivityFn(ctx)
	if err != nil {
		return unknownResult(err)
	}
	if len(first) > 0 {
		return busyResult(first)
	}
	if opts.ProbeDelay <= 0 {
		return idleResult()
	}
	timer := time.NewTimer(opts.ProbeDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return unknownResult(ctx.Err())
	case <-timer.C:
	}
	second, err := opts.ActivityFn(ctx)
	if err != nil {
		return unknownResult(err)
	}
	if len(second) > 0 {
		return busyResult(second)
	}
	return idleResult()
}

// Ensure returns nil only when Check can prove the host is idle.
func Ensure(ctx context.Context, opts Options, action string) error {
	if strings.TrimSpace(action) == "" {
		action = "operacao"
	}
	result := Check(ctx, opts)
	switch result.Status {
	case StatusIdle:
		return nil
	case StatusBusy:
		return fmt.Errorf("%s abortado: host nao esta ocioso (%s)", action, FormatActivities(result.Activities))
	default:
		return fmt.Errorf("%s abortado: nao foi possivel provar host ocioso: %s", action, result.Error)
	}
}

// DefaultActivities reads ps output and extracts known active CI/build process
// patterns.
func DefaultActivities(ctx context.Context) ([]Activity, error) {
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid=,ppid=,comm=,args=").Output()
	if err != nil {
		return nil, err
	}
	return ParseActiveProcesses(string(out), os.Getpid()), nil
}

// ParseActiveProcesses parses `ps -eo pid=,ppid=,comm=,args=` output.
func ParseActiveProcesses(psOutput string, currentPID int) []Activity {
	var activities []Activity
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == currentPID {
			continue
		}
		comm := fields[2]
		args := strings.Join(fields[3:], " ")
		if IsActiveBuildProcess(comm, args) {
			activities = append(activities, Activity{PID: pid, Command: args})
		}
	}
	return activities
}

// IsActiveBuildProcess classifies commands that make mutation unsafe.
func IsActiveBuildProcess(comm, args string) bool {
	if strings.Contains(args, "civmctl cleanup") ||
		strings.Contains(args, "civmctl disk-watchdog") ||
		strings.Contains(args, "civmctl idle-check") {
		return false
	}
	switch {
	case strings.Contains(comm, "Runner.Worker"), strings.Contains(args, "Runner.Worker"):
		return true
	case strings.Contains(args, "Runner.PluginHost"):
		return true
	case strings.Contains(args, "/_work/"):
		return true
	case strings.Contains(args, "docker build"), strings.Contains(args, "docker compose"):
		return true
	case strings.Contains(args, "docker-compose"), strings.Contains(args, "buildx build"):
		return true
	case strings.Contains(args, "buildctl "):
		return true
	}
	return false
}

// FormatActivities returns compact, user-facing evidence.
func FormatActivities(activities []Activity) string {
	limit := len(activities)
	if limit > 3 {
		limit = 3
	}
	parts := make([]string, 0, limit+1)
	for _, a := range activities[:limit] {
		cmd := a.Command
		if len(cmd) > 90 {
			cmd = cmd[:89] + "..."
		}
		parts = append(parts, fmt.Sprintf("pid=%d %s", a.PID, cmd))
	}
	if len(activities) > limit {
		parts = append(parts, fmt.Sprintf("+%d outro(s)", len(activities)-limit))
	}
	return strings.Join(parts, "; ")
}

// Render writes a human-readable report.
func (r Result) Render(w io.Writer) {
	fmt.Fprintf(w, "Host idle-check: %s (exit %d)\n", r.Status, r.ExitCode)
	if r.Error != "" {
		fmt.Fprintf(w, "Erro: %s\n", r.Error)
	}
	if len(r.Activities) > 0 {
		fmt.Fprintln(w, "Atividade detectada:")
		for _, a := range r.Activities {
			fmt.Fprintf(w, "  pid=%d %s\n", a.PID, a.Command)
		}
	}
}

// RenderJSON emits machine-readable output.
func (r Result) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func idleResult() Result {
	return Result{Status: StatusIdle, ExitCode: StatusIdle.ExitCode()}
}

func busyResult(activities []Activity) Result {
	return Result{Status: StatusBusy, ExitCode: StatusBusy.ExitCode(), Activities: activities}
}

func unknownResult(err error) Result {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return Result{Status: StatusUnknown, ExitCode: StatusUnknown.ExitCode(), Error: msg}
}
