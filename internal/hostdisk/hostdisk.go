// Package hostdisk consumes Hyper-V host volume metrics delivered to the guest
// and evaluates free-space floors and headroom invariants for the VHDX volume.
//
// The host writes a JSON snapshot (see deploy/windows/civm-host-metrics.ps1)
// and delivers a copy to the guest at DefaultHostMetricsPath. This package is
// read-only: it never mutates the host, the VM, or any storage. Freshness is
// not guaranteed by the guest, so a stale snapshot or a failed delivery is
// treated as critical (DT-v2-9): an outdated v_free_gb that "looks ok" must not
// hide a host that already crossed a danger floor.
package hostdisk

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
)

const (
	levelOK   = "ok"
	levelWarn = "warn"
	levelCrit = "crit"

	deliveryFailed = "failed"

	reasonFileAbsent     = "host metrics file absent; delivery not yet observed on guest"
	reasonStale          = "host metrics stale; freshness not guaranteed"
	reasonDeliveryFailed = "host metrics delivery failed; host-only snapshot, guest values unavailable"
	reasonReadFailed     = "host metrics read failed"
	reasonParseFailed    = "host metrics parse failed"
	reasonTimestampParse = "host metrics timestamp not RFC3339"
	reasonMissingTime    = "host metrics missing timestamp"
)

// Metrics is the host-side volume snapshot delivered to the guest. All sizes
// are whole gibibytes as measured on the Windows host (DT-v2-11).
type Metrics struct {
	// VFreeGB is the free space currently available on the host V: volume.
	VFreeGB int64 `json:"v_free_gb"`
	// VSizeGB is the total capacity of the host V: volume.
	VSizeGB int64 `json:"v_size_gb"`
	// VHDXFileSizeGB is the current on-disk size of the VHDX file.
	VHDXFileSizeGB int64 `json:"vhdx_file_size_gb"`
	// VHDXMinSizeGB is the minimum size of the VHDX as reported by Get-VHD.
	VHDXMinSizeGB int64 `json:"vhdx_min_size_gb"`
	// VHDXMaxSizeGB is the maximum configured size of the VHDX (Get-VHD).
	VHDXMaxSizeGB int64 `json:"vhdx_max_size_gb"`
	// VHDXBlockSizeBytes is the dynamic VHDX block size. Values above 1 MiB are
	// a known reclaim blocker on this host class.
	VHDXBlockSizeBytes int64 `json:"vhdx_block_size_bytes,omitempty"`
	// GuestFreeGB is the guest root filesystem free space, gathered by the host
	// task over SSH. It is 0 when delivery failed (host-only snapshot).
	GuestFreeGB int64 `json:"guest_free_gb"`
	// GapGB is VHDXFileSizeGB minus the guest used space, i.e. the reclaimable
	// slack the VHDX still holds versus what the guest actually consumes.
	GapGB int64 `json:"gap_gb"`
	// Timestamp is the RFC3339 instant the host wrote this snapshot.
	Timestamp string `json:"timestamp"`
	// DeliveryStatus is "failed" when the host could not reach the guest over
	// SSH and wrote a host-only snapshot; empty/omitted on success (DT-v2-5).
	DeliveryStatus string `json:"delivery_status,omitempty"`
}

// Report is the evaluated host-disk status returned to the CLI guard.
type Report struct {
	Metrics
	// Stale is true when the snapshot is older than MaxAge or absent/unreadable.
	Stale bool `json:"stale"`
	// Level is the severity classification: ok, warn or crit.
	Level string `json:"level"`
	// FreeHeadroomViolation is true when VFreeGB < HeadroomGB: too little host
	// free space to safely run Optimize-VHD scratch growth (DT-v2-11).
	FreeHeadroomViolation bool `json:"free_headroom_violation"`
	// AllocationHeadroomViolation is true when VSizeGB-VHDXMaxSizeGB < HeadroomGB:
	// the volume cannot absorb the VHDX growing to its configured maximum.
	AllocationHeadroomViolation bool `json:"allocation_headroom_violation"`
	// Reason carries human-facing context for non-ok states.
	Reason string `json:"reason,omitempty"`
}

// Options controls metric ingestion and thresholds. All I/O is injectable so
// unit tests run without touching the filesystem or the clock.
type Options struct {
	Path       string
	MaxAge     time.Duration
	WarnFreeGB int64
	CritFreeGB int64
	HeadroomGB int64
	ReadFileFn func(path string) ([]byte, error)
	NowFn      func() time.Time
}

// DefaultOptions returns production defaults backed by civm constants.
func DefaultOptions() Options {
	return Options{
		Path:       civm.DefaultHostMetricsPath,
		MaxAge:     time.Duration(civm.DefaultHostMetricsMaxAgeMinutes) * time.Minute,
		WarnFreeGB: civm.DefaultHostVolumeWarnFreeGB,
		CritFreeGB: civm.DefaultHostVolumeCritFreeGB,
		HeadroomGB: civm.DefaultHostVolumeHeadroomGB,
		ReadFileFn: os.ReadFile,
		NowFn:      time.Now,
	}
}

func applyDefaults(opts *Options) {
	if opts.Path == "" {
		opts.Path = civm.DefaultHostMetricsPath
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = time.Duration(civm.DefaultHostMetricsMaxAgeMinutes) * time.Minute
	}
	if opts.WarnFreeGB == 0 {
		opts.WarnFreeGB = civm.DefaultHostVolumeWarnFreeGB
	}
	if opts.CritFreeGB == 0 {
		opts.CritFreeGB = civm.DefaultHostVolumeCritFreeGB
	}
	if opts.HeadroomGB == 0 {
		opts.HeadroomGB = civm.DefaultHostVolumeHeadroomGB
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = os.ReadFile
	}
	if opts.NowFn == nil {
		opts.NowFn = time.Now
	}
}

// Check reads the delivered host metrics and classifies the host volume state.
// An absent file, a read/parse failure, a stale snapshot, or a failed delivery
// all yield Stale and Level=crit (DT-v2-9): the guest cannot prove freshness,
// so it must fail safe rather than trust a possibly outdated v_free_gb.
func Check(opts Options) (Report, error) {
	applyDefaults(&opts)

	data, err := opts.ReadFileFn(opts.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return staleReport(Metrics{}, reasonFileAbsent), nil
		}
		return staleReport(Metrics{}, reasonReadFailed), fmt.Errorf("read host metrics %q: %w", opts.Path, err)
	}

	var m Metrics
	if err := json.Unmarshal(data, &m); err != nil {
		return staleReport(Metrics{}, reasonParseFailed), fmt.Errorf("parse host metrics %q: %w", opts.Path, err)
	}

	r := Report{Metrics: m}
	r.FreeHeadroomViolation = m.VFreeGB < opts.HeadroomGB
	r.AllocationHeadroomViolation = m.VSizeGB-m.VHDXMaxSizeGB < opts.HeadroomGB

	stale, staleReason := evaluateFreshness(m, opts)
	r.Stale = stale

	switch {
	case stale:
		r.Level = levelCrit
		r.Reason = staleReason
	case m.DeliveryStatus == deliveryFailed:
		r.Level = levelCrit
		r.Reason = reasonDeliveryFailed
	default:
		r.Level = levelByFree(m.VFreeGB, opts)
		switch r.Level {
		case levelCrit:
			r.Reason = fmt.Sprintf("v_free_gb=%d below crit floor %dGB", m.VFreeGB, opts.CritFreeGB)
		case levelWarn:
			r.Reason = fmt.Sprintf("v_free_gb=%d below warn floor %dGB", m.VFreeGB, opts.WarnFreeGB)
		}
	}
	return r, nil
}

func evaluateFreshness(m Metrics, opts Options) (bool, string) {
	if m.Timestamp == "" {
		return true, reasonMissingTime
	}
	ts, err := time.Parse(time.RFC3339, m.Timestamp)
	if err != nil {
		return true, reasonTimestampParse
	}
	if opts.NowFn().Sub(ts) > opts.MaxAge {
		return true, reasonStale
	}
	return false, ""
}

func levelByFree(freeGB int64, opts Options) string {
	switch {
	case freeGB <= opts.CritFreeGB:
		return levelCrit
	case freeGB <= opts.WarnFreeGB:
		return levelWarn
	default:
		return levelOK
	}
}

func staleReport(m Metrics, reason string) Report {
	return Report{Metrics: m, Stale: true, Level: levelCrit, Reason: reason}
}

// WantsCleanup reports whether the host volume is under enough pressure (warn or
// crit) that a guest-side consumer should run aggressive disk cleanup, even when
// the GUEST filesystem percentage alone would not trigger it. This is the
// host-aware half of the job-started gate: the guest can sit at a comfortable
// percentage while the host V: volume already crossed a floor (the 2026-06
// PausedCritical incident — guest 69%, host V: 88%).
func (r Report) WantsCleanup() bool {
	return r.Level == levelWarn || r.Level == levelCrit
}

// Blocks reports whether a job MUST be rejected: the host is genuinely critical
// AND the snapshot is fresh. A stale/absent snapshot is Level=crit by fail-safe
// (DT-v2-9) but does NOT block here — missing telemetry is an infra gap, not
// proof the disk is full, and blocking every job on a missing metrics file would
// be a self-inflicted CI outage. Cleanup still runs on stale (WantsCleanup), but
// only fresh evidence of a full host stops the job.
func (r Report) Blocks() bool {
	return r.Level == levelCrit && !r.Stale
}

// RenderJSON emits the report as indented machine-readable JSON.
func RenderJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// RenderText writes a compact human-readable summary.
func RenderText(w io.Writer, r Report) {
	fmt.Fprintf(w, "Host disk: level=%s stale=%t v_free=%dGB/%dGB vhdx_file=%dGB vhdx_max=%dGB guest_free=%dGB gap=%dGB\n",
		r.Level, r.Stale, r.VFreeGB, r.VSizeGB, r.VHDXFileSizeGB, r.VHDXMaxSizeGB, r.GuestFreeGB, r.GapGB)
	if r.VHDXBlockSizeBytes > 0 {
		fmt.Fprintf(w, "VHDXBlockSizeBytes: %d\n", r.VHDXBlockSizeBytes)
	}
	if r.FreeHeadroomViolation {
		fmt.Fprintln(w, "FreeHeadroomViolation: v_free below Optimize-VHD scratch headroom")
	}
	if r.AllocationHeadroomViolation {
		fmt.Fprintln(w, "AllocationHeadroomViolation: V: cannot absorb VHDX growth to configured max")
	}
	if r.Reason != "" {
		fmt.Fprintf(w, "Reason: %s\n", r.Reason)
	}
}
