// Package reversewatchdog detects when a systemd timer has stopped firing.
// Useful as alarm for civmctl-disk-watchdog (which itself protects disk):
// if hourly watchdog hasn't logged in >2h, something is wrong with the
// watchdog (timer disabled, journal corrupted, OOM killed). Stdlib-only.
package reversewatchdog

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
)

// Status of a single timer's last-fire age.
type Status int

const (
	StatusOK      Status = iota // last fire within MaxAge
	StatusStale                 // last fire too old (timer parado?)
	StatusUnknown               // journal vazio ou erro
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusStale:
		return "stale"
	case StatusUnknown:
		return "unknown"
	}
	return "?"
}

// ExitCode mapeia para 0/1/2.
func (s Status) ExitCode() int {
	switch s {
	case StatusOK:
		return 0
	case StatusStale:
		return 1
	}
	return 2
}

// Result captures check outcome.
type Result struct {
	Unit        string
	MaxAge      time.Duration
	Status      Status
	LastFireAgo string // "12h", "30m", or "never"
	Err         error
}

// Options control the check.
type Options struct {
	Unit   string        // ex: "civmctl-disk-watchdog.service"
	MaxAge time.Duration // default 2h
	RunFn  func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultOptions returns sane defaults.
func DefaultOptions() Options {
	return Options{
		Unit:   "civmctl-disk-watchdog.service",
		MaxAge: time.Duration(civm.DefaultReverseMaxAgeHours) * time.Hour,
		RunFn:  defaultRun,
	}
}

// Check queries journalctl for the most recent run of opts.Unit and
// returns Stale if older than MaxAge. Uses systemctl show TimersCalendar
// pra fallback se journal vazio.
func Check(ctx context.Context, opts Options) Result {
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.MaxAge == 0 {
		opts.MaxAge = time.Duration(civm.DefaultReverseMaxAgeHours) * time.Hour
	}
	r := Result{Unit: opts.Unit, MaxAge: opts.MaxAge}
	if err := civm.ValidateServiceUnit(opts.Unit); err != nil {
		r.Status = StatusUnknown
		r.LastFireAgo = "never"
		r.Err = err
		return r
	}

	// Use systemctl show pra LastTriggerUSec do timer (mais confiavel que journal)
	timerUnit := strings.TrimSuffix(opts.Unit, ".service") + ".timer"
	out, err := opts.RunFn(ctx, "systemctl", "show", timerUnit, "--property=LastTriggerUSec")
	if err != nil {
		r.Status = StatusUnknown
		r.LastFireAgo = "never (timer nao instalado?)"
		r.Err = fmt.Errorf("systemctl show: %w", err)
		return r
	}
	last, ok := parseLastTriggerUSec(string(out))
	if !ok {
		r.Status = StatusUnknown
		r.LastFireAgo = "never"
		return r
	}
	age := time.Since(last)
	r.LastFireAgo = roundDur(age)
	if age > opts.MaxAge {
		r.Status = StatusStale
	} else {
		r.Status = StatusOK
	}
	return r
}

// parseLastTriggerUSec extracts time from "LastTriggerUSec=Sun 2026-05-10
// 15:00:25 UTC" output of systemctl show.
func parseLastTriggerUSec(s string) (time.Time, bool) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		const prefix = "LastTriggerUSec="
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		val := strings.TrimPrefix(line, prefix)
		if val == "" || val == "n/a" || val == "0" {
			return time.Time{}, false
		}
		// Format: "Sun 2026-05-10 15:00:25 UTC"
		layouts := []string{
			"Mon 2006-01-02 15:04:05 MST",
			"Mon 2006-01-02 15:04:05 -0700",
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, val); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

func roundDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Render writes a brief summary.
func (r Result) Render(w io.Writer) {
	fmt.Fprintf(w, "Reverse-watchdog: %s\n", r.Unit)
	fmt.Fprintf(w, "Max age threshold: %s | Last fire: %s\n", r.MaxAge, r.LastFireAgo)
	fmt.Fprintf(w, "Status: %s (exit %d)\n", r.Status, r.Status.ExitCode())
	if r.Err != nil {
		fmt.Fprintf(w, "Erro: %v\n", r.Err)
	}
	switch r.Status {
	case StatusStale:
		fmt.Fprintln(w, "ATENCAO: timer parou de disparar. Verificar:")
		fmt.Fprintf(w, "  systemctl status %s\n", r.Unit)
		fmt.Fprintf(w, "  journalctl -u %s --since='%s ago'\n", r.Unit, r.MaxAge)
	case StatusUnknown:
		fmt.Fprintln(w, "Timer ausente OU nunca disparou. Instalar:")
		fmt.Fprintln(w, "  sudo systemctl enable --now civmctl-disk-watchdog.timer")
	}
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
