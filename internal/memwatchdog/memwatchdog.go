// Package memwatchdog samples /proc/meminfo and classifies memory pressure
// (MemAvailable% + swap usage), so civm records and identifies OOM/thrash risk
// in real time. The key signal is MemAvailable (free + reclaimable cache) and
// swap-in-use — NOT "free", which is misleading on Linux (the kernel uses idle
// RAM as page cache). Stdlib-only; MeminfoFn is injected for hermetic tests.
package memwatchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Decision is the watchdog outcome; its int value doubles as the exit code.
type Decision int

const (
	DecisionOK       Decision = iota // 0 — memory healthy
	DecisionWarn                     // 1 — pressure building (degraded margin)
	DecisionCritical                 // 2 — OOM/thrash risk
)

func (d Decision) String() string {
	switch d {
	case DecisionOK:
		return "ok"
	case DecisionWarn:
		return "warn"
	case DecisionCritical:
		return "critical"
	}
	return "?"
}

// ExitCode maps the decision to a process exit code (0/1/2).
func (d Decision) ExitCode() int { return int(d) }

// Meminfo is the parsed, MB-normalized subset of /proc/meminfo we act on.
type Meminfo struct {
	MemTotalMB     int64 `json:"mem_total_mb"`
	MemAvailableMB int64 `json:"mem_available_mb"`
	AvailPct       int   `json:"avail_pct"`
	SwapTotalMB    int64 `json:"swap_total_mb"`
	SwapUsedMB     int64 `json:"swap_used_mb"`
}

// Options configure the thresholds. Zero values fall back to DefaultOptions.
type Options struct {
	WarnAvailPct int   // warn when MemAvailable < this % of total (default 15)
	CritAvailPct int   // critical when MemAvailable < this % (default 8)
	WarnSwapMB   int64 // warn when swap-in-use exceeds this (default 512)
	CritSwapMB   int64 // critical when swap-in-use exceeds this (default 1536)
	MeminfoFn    func() (string, error)
	NowFn        func() time.Time
}

// DefaultOptions returns production thresholds tuned for the civm runner VM.
func DefaultOptions() Options {
	return Options{
		WarnAvailPct: 15,
		CritAvailPct: 8,
		WarnSwapMB:   512,
		CritSwapMB:   1536,
		MeminfoFn:    func() (string, error) { b, err := os.ReadFile("/proc/meminfo"); return string(b), err },
		NowFn:        time.Now,
	}
}

// Result is the classified outcome.
type Result struct {
	Time     time.Time `json:"time"`
	Decision Decision  `json:"-"`
	Status   string    `json:"status"`
	Mem      Meminfo   `json:"mem"`
	Reason   string    `json:"reason,omitempty"`
	Err      string    `json:"error,omitempty"`
}

// Check reads and classifies memory pressure.
func Check(ctx context.Context, opts Options) Result {
	applyDefaults(&opts)
	res := Result{Time: opts.NowFn().UTC()}
	raw, err := opts.MeminfoFn()
	if err != nil {
		res.Decision = DecisionCritical
		res.Status = res.Decision.String()
		res.Err = err.Error()
		res.Reason = "meminfo-read-failed"
		return res
	}
	mem, err := parseMeminfo(raw)
	if err != nil {
		res.Decision = DecisionCritical
		res.Status = res.Decision.String()
		res.Err = err.Error()
		res.Reason = "meminfo-parse-failed"
		return res
	}
	res.Mem = mem
	res.Decision, res.Reason = classify(mem, opts)
	res.Status = res.Decision.String()
	return res
}

// Sample reads /proc/meminfo (via MeminfoFn) and returns the parsed, MB-
// normalized Meminfo without classifying pressure. Admission uses it to compute
// the generous MemoryMax (MemTotal − host)/MaxHeavy without duplicating the
// parser. A read or parse failure is returned to the caller (no thresholds, no
// Decision). Check/thresholds are untouched.
func Sample(opts Options) (Meminfo, error) {
	applyDefaults(&opts)
	raw, err := opts.MeminfoFn()
	if err != nil {
		return Meminfo{}, fmt.Errorf("memwatchdog: ler meminfo: %w", err)
	}
	mem, err := parseMeminfo(raw)
	if err != nil {
		return Meminfo{}, fmt.Errorf("memwatchdog: parse meminfo: %w", err)
	}
	return mem, nil
}

func classify(m Meminfo, opts Options) (Decision, string) {
	if m.AvailPct < opts.CritAvailPct {
		return DecisionCritical, fmt.Sprintf("mem-available %d%% < %d%%", m.AvailPct, opts.CritAvailPct)
	}
	if m.SwapUsedMB > opts.CritSwapMB {
		return DecisionCritical, fmt.Sprintf("swap-used %dMB > %dMB (thrash)", m.SwapUsedMB, opts.CritSwapMB)
	}
	if m.AvailPct < opts.WarnAvailPct {
		return DecisionWarn, fmt.Sprintf("mem-available %d%% < %d%%", m.AvailPct, opts.WarnAvailPct)
	}
	if m.SwapUsedMB > opts.WarnSwapMB {
		return DecisionWarn, fmt.Sprintf("swap-used %dMB > %dMB", m.SwapUsedMB, opts.WarnSwapMB)
	}
	return DecisionOK, ""
}

// parseMeminfo extracts MemTotal, MemAvailable, SwapTotal, SwapFree (all in kB
// in /proc/meminfo) and normalizes to MB + an availability percentage.
func parseMeminfo(s string) (Meminfo, error) {
	vals := map[string]int64{}
	for _, line := range strings.Split(s, "\n") {
		key, kb, ok := parseMeminfoLine(line)
		if ok {
			vals[key] = kb
		}
	}
	total, okT := vals["MemTotal"]
	avail, okA := vals["MemAvailable"]
	if !okT || !okA || total <= 0 {
		return Meminfo{}, fmt.Errorf("meminfo missing MemTotal/MemAvailable")
	}
	swapTotal := vals["SwapTotal"]
	swapUsed := max(swapTotal-vals["SwapFree"], 0)
	return Meminfo{
		MemTotalMB:     total / 1024,
		MemAvailableMB: avail / 1024,
		AvailPct:       int(avail * 100 / total),
		SwapTotalMB:    swapTotal / 1024,
		SwapUsedMB:     swapUsed / 1024,
	}, nil
}

func parseMeminfoLine(line string) (key string, kb int64, ok bool) {
	name, rest, found := strings.Cut(line, ":")
	if !found {
		return "", 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 {
		return "", 0, false
	}
	v, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(name), v, true
}

func applyDefaults(opts *Options) {
	d := DefaultOptions()
	if opts.WarnAvailPct == 0 {
		opts.WarnAvailPct = d.WarnAvailPct
	}
	if opts.CritAvailPct == 0 {
		opts.CritAvailPct = d.CritAvailPct
	}
	if opts.WarnSwapMB == 0 {
		opts.WarnSwapMB = d.WarnSwapMB
	}
	if opts.CritSwapMB == 0 {
		opts.CritSwapMB = d.CritSwapMB
	}
	if opts.MeminfoFn == nil {
		opts.MeminfoFn = d.MeminfoFn
	}
	if opts.NowFn == nil {
		opts.NowFn = d.NowFn
	}
}

// RenderJSON writes the result as one JSON line — the record civm logs to the
// journal each tick (and what an external scraper can ingest).
func (r Result) RenderJSON(w io.Writer) error {
	return json.NewEncoder(w).Encode(r)
}

// Render writes a human-readable line.
func (r Result) Render(w io.Writer) {
	fmt.Fprintf(w, "civmctl mem-watchdog: %s | MemAvailable=%dMB (%d%%) | swap=%dMB/%dMB",
		strings.ToUpper(r.Status), r.Mem.MemAvailableMB, r.Mem.AvailPct, r.Mem.SwapUsedMB, r.Mem.SwapTotalMB)
	if r.Reason != "" {
		fmt.Fprintf(w, " | %s", r.Reason)
	}
	fmt.Fprintln(w)
}
