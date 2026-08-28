// Package diskdoctor diagnoses why the guest discard/fstrim pipeline reclaims
// (or fails to reclaim) space for the host VHDX. It is read-only by default:
// it inspects findmnt, /proc/mounts, lsblk and /sys, and only the opt-in
// reference test allocates and frees a scratch file. It never mutates mounts,
// controllers or partitions.
package diskdoctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Controller classifications surfaced in Report.Controller.
const (
	ControllerSCSI    = "scsi"
	ControllerIDE     = "ide"
	ControllerVirtio  = "virtio"
	ControllerUnknown = "unknown"
)

// RootCause messages, ordered by the DT-v2-10 decision tree.
const (
	rootCauseDiscardDisabled   = "discard disabled on mount"
	rootCauseIDEController     = "IDE controller does not propagate UNMAP"
	rootCauseTrimNotAdvertised = "TRIM not advertised"
	rootCauseTrimSupported     = "TRIM supported, online shrink expected"
)

const (
	defaultRootPath = "/"
	procMountsPath  = "/proc/mounts"
	discardOption   = "discard"
)

// Report is the read-only disk-doctor diagnosis.
type Report struct {
	Device                string `json:"device"`
	Controller            string `json:"controller"` // scsi|ide|virtio|unknown
	MountDiscard          bool   `json:"mount_discard"`
	DiscGranBytes         int64  `json:"disc_gran_bytes"`
	DiscMaxBytes          int64  `json:"disc_max_bytes"`
	TrimEffective         bool   `json:"trim_effective"`
	RootCause             string `json:"root_cause"`
	HostHeadroomViolation bool   `json:"host_headroom_violation,omitempty"`

	// ReferenceDeltaBytes is the measured shrink from the opt-in reference
	// test (allocate 100 MB, free, fstrim, measure). Zero when not run.
	ReferenceDeltaBytes int64 `json:"reference_delta_bytes,omitempty"`
}

// Options injects every side effect so unit tests run with no real syscalls,
// exec or filesystem access.
type Options struct {
	RootPath        string
	HostMetricsPath string

	// HostHeadroomGB, when > 0, enables the host headroom check against the
	// v_free_gb field of the host-metrics JSON at HostMetricsPath. Defaults to
	// 0 (disabled) so diskdoctor stays self-contained.
	HostHeadroomGB int64

	// ReferenceTest, when true, runs the allocate/free/fstrim delta via RunFn.
	ReferenceTest bool

	ReadFileFn func(path string) ([]byte, error)
	RunFn      func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultOptions returns production defaults that probe the real host.
func DefaultOptions() Options {
	return Options{
		RootPath:   defaultRootPath,
		ReadFileFn: os.ReadFile,
		RunFn:      defaultRun,
	}
}

// hostMetrics is the minimal subset of the host-metrics JSON consumed here.
type hostMetrics struct {
	VFreeGB int64 `json:"v_free_gb"`
}

type findmntOutput struct {
	Filesystems []findmntFilesystem `json:"filesystems"`
}

type findmntFilesystem struct {
	Target  string `json:"target"`
	Source  string `json:"source"`
	FsType  string `json:"fstype"`
	Options string `json:"options"`
}

// Diagnose resolves the device backing RootPath and applies the DT-v2-10
// decision tree to compose RootCause. It returns an error only when RootPath
// is not mounted on a resolvable device.
func Diagnose(ctx context.Context, opts Options) (Report, error) {
	applyDefaults(&opts)

	device, err := resolveDevice(ctx, opts)
	if err != nil {
		return Report{}, err
	}
	report := Report{Device: device}
	report.MountDiscard = mountHasDiscard(opts, device)
	report.Controller = classifyController(ctx, opts, device)
	report.DiscGranBytes, report.DiscMaxBytes = discardLimits(ctx, opts, device)
	report.TrimEffective = report.DiscMaxBytes > 0
	report.RootCause = composeRootCause(report)
	report.HostHeadroomViolation = hostHeadroomViolation(opts)

	if opts.ReferenceTest {
		report.ReferenceDeltaBytes = referenceDelta(ctx, opts)
	}
	return report, nil
}

func applyDefaults(opts *Options) {
	if strings.TrimSpace(opts.RootPath) == "" {
		opts.RootPath = defaultRootPath
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = os.ReadFile
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
}

// resolveDevice runs `findmnt --json <RootPath>` and returns the backing
// device path (e.g. /dev/sdd). DT-v2-10 step (1): not mounted → error.
func resolveDevice(ctx context.Context, opts Options) (string, error) {
	out, err := opts.RunFn(ctx, "findmnt", "--json", "--target", opts.RootPath)
	if err != nil {
		return "", fmt.Errorf("findmnt %q: %w", opts.RootPath, err)
	}
	var parsed findmntOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", fmt.Errorf("parse findmnt output for %q: %w", opts.RootPath, err)
	}
	for _, fs := range parsed.Filesystems {
		if fs.Target == opts.RootPath && strings.TrimSpace(fs.Source) != "" {
			return strings.TrimSpace(fs.Source), nil
		}
	}
	// Fall back to the first filesystem with a non-empty source when findmnt
	// resolved a parent mount for --target.
	for _, fs := range parsed.Filesystems {
		if strings.TrimSpace(fs.Source) != "" {
			return strings.TrimSpace(fs.Source), nil
		}
	}
	return "", fmt.Errorf("device not mounted on %q", opts.RootPath)
}

// mountHasDiscard reads /proc/mounts and reports whether the device line
// carries the discard option. DT-v2-10 step (2).
func mountHasDiscard(opts Options, device string) bool {
	data, err := opts.ReadFileFn(procMountsPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[0] != device {
			continue
		}
		for _, opt := range strings.Split(fields[3], ",") {
			if opt == discardOption {
				return true
			}
		}
	}
	return false
}

// classifyController maps the transport reported by `lsblk -o NAME,TRAN` (with
// a /sys/block fallback) to scsi|ide|virtio|unknown. DT-v2-10 step (3).
func classifyController(ctx context.Context, opts Options, device string) string {
	base := deviceBaseName(device)
	if tran := lsblkTransport(ctx, opts, base); tran != "" {
		if c := normalizeTransport(tran); c != ControllerUnknown {
			return c
		}
	}
	if c := sysBlockController(opts, base); c != ControllerUnknown {
		return c
	}
	return ControllerUnknown
}

func lsblkTransport(ctx context.Context, opts Options, base string) string {
	out, err := opts.RunFn(ctx, "lsblk", "-dno", "TRAN", filepath.Join("/dev", base))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(out)))
}

func normalizeTransport(tran string) string {
	switch tran {
	case "sata", "scsi", "sas":
		return ControllerSCSI
	case "ata", "ide":
		return ControllerIDE
	case "virtio":
		return ControllerVirtio
	}
	return ControllerUnknown
}

// sysBlockController inspects the /sys/block/<base>/device symlink target as a
// fallback when lsblk does not report a transport.
func sysBlockController(opts Options, base string) string {
	data, err := opts.ReadFileFn(filepath.Join("/sys/block", base, "device", "modalias"))
	if err != nil {
		return ControllerUnknown
	}
	modalias := strings.ToLower(string(data))
	switch {
	case strings.Contains(modalias, "virtio"):
		return ControllerVirtio
	case strings.Contains(modalias, "scsi"):
		return ControllerSCSI
	case strings.Contains(modalias, "ide"), strings.Contains(modalias, "ata"):
		return ControllerIDE
	}
	return ControllerUnknown
}

// discardLimits parses `lsblk -D -b -o NAME,DISC-GRAN,DISC-MAX` for the device
// base name and returns granularity and max bytes. DT-v2-10 steps (4)/(5).
func discardLimits(ctx context.Context, opts Options, device string) (gran, max int64) {
	base := deviceBaseName(device)
	out, err := opts.RunFn(ctx, "lsblk", "-D", "-b", "-n", "-o", "NAME,DISC-GRAN,DISC-MAX", filepath.Join("/dev", base))
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if strings.TrimPrefix(fields[0], "/dev/") != base && fields[0] != base {
			continue
		}
		gran = parseInt64(fields[1])
		max = parseInt64(fields[2])
		return gran, max
	}
	return 0, 0
}

// composeRootCause applies the DT-v2-10 decision tree in order on the already
// populated report fields. resolveDevice handled step (1).
func composeRootCause(r Report) string {
	if !r.MountDiscard {
		return rootCauseDiscardDisabled
	}
	if r.Controller == ControllerIDE {
		return rootCauseIDEController
	}
	if r.DiscMaxBytes == 0 {
		return rootCauseTrimNotAdvertised
	}
	return rootCauseTrimSupported
}

// hostHeadroomViolation reports whether the host V: free space dropped below
// the configured headroom. Disabled unless HostHeadroomGB > 0 and a metrics
// file is readable.
func hostHeadroomViolation(opts Options) bool {
	if opts.HostHeadroomGB <= 0 || strings.TrimSpace(opts.HostMetricsPath) == "" {
		return false
	}
	data, err := opts.ReadFileFn(opts.HostMetricsPath)
	if err != nil {
		return false
	}
	var m hostMetrics
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	return m.VFreeGB < opts.HostHeadroomGB
}

// referenceDelta runs the opt-in allocate/free/fstrim delta entirely through
// RunFn so it stays injectable. It returns the reclaimed bytes parsed from the
// helper, or 0 on any failure.
func referenceDelta(ctx context.Context, opts Options) int64 {
	out, err := opts.RunFn(ctx, "civm-reference-test")
	if err != nil {
		return 0
	}
	return parseInt64(strings.TrimSpace(string(out)))
}

func deviceBaseName(device string) string {
	return filepath.Base(strings.TrimSpace(device))
}

func parseInt64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// RenderJSON emits the indented machine-readable report.
func RenderJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// RenderText writes a human-readable summary.
func RenderText(w io.Writer, r Report) {
	fmt.Fprintf(w, "Disk-doctor: device=%s controller=%s discard=%v trim_effective=%v\n",
		r.Device, r.Controller, r.MountDiscard, r.TrimEffective)
	fmt.Fprintf(w, "  disc-gran=%d bytes  disc-max=%d bytes\n", r.DiscGranBytes, r.DiscMaxBytes)
	fmt.Fprintf(w, "  root_cause: %s\n", r.RootCause)
	if r.HostHeadroomViolation {
		fmt.Fprintln(w, "  host_headroom_violation: true")
	}
	if r.ReferenceDeltaBytes > 0 {
		fmt.Fprintf(w, "  reference_delta: %d bytes\n", r.ReferenceDeltaBytes)
	}
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
