package diskdoctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	testDevice  = "/dev/sdd"
	testBase    = "sdd"
	procDiscard = "/dev/sdd / ext4 rw,relatime,discard,errors=remount-ro 0 0\n"
	procNoDisc  = "/dev/sdd / ext4 rw,relatime,errors=remount-ro 0 0\n"
)

var errRun = errors.New("exec failed")

const findmntJSON = `{"filesystems":[{"target":"/","source":"/dev/sdd","fstype":"ext4","options":"rw,relatime,discard"}]}`

// stubRun builds a RunFn dispatching by command name with canned outputs.
func stubRun(findmnt, tran, lsblkD string, runErr map[string]error) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if err := runErr[name]; err != nil {
			return nil, err
		}
		switch name {
		case "findmnt":
			return []byte(findmnt), nil
		case "lsblk":
			if containsArg(args, "TRAN") {
				return []byte(tran), nil
			}
			return []byte(lsblkD), nil
		}
		return nil, nil
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestDiagnoseDecisionTree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		proc          string
		tran          string
		lsblkD        string
		wantCtrl      string
		wantDiscard   bool
		wantMax       int64
		wantTrim      bool
		wantRootCause string
	}{
		{
			name:          "discard disabled wins first",
			proc:          procNoDisc,
			tran:          "sata\n",
			lsblkD:        "sdd 1048576 4294966784\n",
			wantCtrl:      ControllerSCSI,
			wantDiscard:   false,
			wantMax:       4294966784,
			wantTrim:      true,
			wantRootCause: rootCauseDiscardDisabled,
		},
		{
			name:          "ide controller with discard on",
			proc:          procDiscard,
			tran:          "ata\n",
			lsblkD:        "sdd 1048576 4294966784\n",
			wantCtrl:      ControllerIDE,
			wantDiscard:   true,
			wantMax:       4294966784,
			wantTrim:      true,
			wantRootCause: rootCauseIDEController,
		},
		{
			name:          "trim not advertised disc-max zero",
			proc:          procDiscard,
			tran:          "scsi\n",
			lsblkD:        "sdd 0 0\n",
			wantCtrl:      ControllerSCSI,
			wantDiscard:   true,
			wantMax:       0,
			wantTrim:      false,
			wantRootCause: rootCauseTrimNotAdvertised,
		},
		{
			name:          "trim supported scsi disc-max positive",
			proc:          procDiscard,
			tran:          "scsi\n",
			lsblkD:        "sdd 1048576 4294966784\n",
			wantCtrl:      ControllerSCSI,
			wantDiscard:   true,
			wantMax:       4294966784,
			wantTrim:      true,
			wantRootCause: rootCauseTrimSupported,
		},
		{
			name:          "virtio transport classified",
			proc:          procDiscard,
			tran:          "virtio\n",
			lsblkD:        "sdd 1048576 4294966784\n",
			wantCtrl:      ControllerVirtio,
			wantDiscard:   true,
			wantMax:       4294966784,
			wantTrim:      true,
			wantRootCause: rootCauseTrimSupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{
				RootPath:   "/",
				ReadFileFn: func(string) ([]byte, error) { return []byte(tt.proc), nil },
				RunFn:      stubRun(findmntJSON, tt.tran, tt.lsblkD, nil),
			}
			r, err := Diagnose(context.Background(), opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.Device != testDevice {
				t.Fatalf("device = %q, want %q", r.Device, testDevice)
			}
			if r.Controller != tt.wantCtrl {
				t.Fatalf("controller = %q, want %q", r.Controller, tt.wantCtrl)
			}
			if r.MountDiscard != tt.wantDiscard {
				t.Fatalf("mount_discard = %v, want %v", r.MountDiscard, tt.wantDiscard)
			}
			if r.DiscMaxBytes != tt.wantMax {
				t.Fatalf("disc_max = %d, want %d", r.DiscMaxBytes, tt.wantMax)
			}
			if r.TrimEffective != tt.wantTrim {
				t.Fatalf("trim_effective = %v, want %v", r.TrimEffective, tt.wantTrim)
			}
			if r.RootCause != tt.wantRootCause {
				t.Fatalf("root_cause = %q, want %q", r.RootCause, tt.wantRootCause)
			}
		})
	}
}

func TestDiagnoseNotMountedReturnsError(t *testing.T) {
	t.Parallel()
	opts := Options{
		RootPath:   "/missing",
		ReadFileFn: func(string) ([]byte, error) { return []byte(procDiscard), nil },
		RunFn: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "findmnt" {
				return []byte(`{"filesystems":[]}`), nil
			}
			return nil, nil
		},
	}
	if _, err := Diagnose(context.Background(), opts); err == nil {
		t.Fatal("expected error when device not mounted on RootPath")
	}
}

func TestDiagnoseFindmntRunFails(t *testing.T) {
	t.Parallel()
	opts := Options{
		RootPath:   "/",
		ReadFileFn: func(string) ([]byte, error) { return []byte(procDiscard), nil },
		RunFn:      stubRun("", "", "", map[string]error{"findmnt": errRun}),
	}
	_, err := Diagnose(context.Background(), opts)
	if err == nil || !errors.Is(err, errRun) {
		t.Fatalf("expected wrapped errRun, got %v", err)
	}
}

func TestDiagnoseFindmntBadJSON(t *testing.T) {
	t.Parallel()
	opts := Options{
		RootPath:   "/",
		ReadFileFn: func(string) ([]byte, error) { return []byte(procDiscard), nil },
		RunFn:      stubRun("{not json", "", "", nil),
	}
	if _, err := Diagnose(context.Background(), opts); err == nil {
		t.Fatal("expected parse error for malformed findmnt JSON")
	}
}

func TestDiagnoseControllerFallbackToSysBlock(t *testing.T) {
	t.Parallel()
	// lsblk TRAN empty → fall back to /sys/block modalias virtio.
	opts := Options{
		RootPath: "/",
		ReadFileFn: func(path string) ([]byte, error) {
			if path == procMountsPath {
				return []byte(procDiscard), nil
			}
			if strings.Contains(path, "modalias") {
				return []byte("virtio:d00000002v00001AF4"), nil
			}
			return nil, errors.New("not found")
		},
		RunFn: stubRun(findmntJSON, "\n", "sdd 1048576 4294966784\n", nil),
	}
	r, err := Diagnose(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Controller != ControllerVirtio {
		t.Fatalf("controller = %q, want %q from /sys fallback", r.Controller, ControllerVirtio)
	}
}

func TestDiagnoseControllerUnknownWhenAllFail(t *testing.T) {
	t.Parallel()
	opts := Options{
		RootPath: "/",
		ReadFileFn: func(path string) ([]byte, error) {
			if path == procMountsPath {
				return []byte(procDiscard), nil
			}
			return nil, errors.New("no sys block")
		},
		RunFn: stubRun(findmntJSON, "\n", "sdd 1048576 4294966784\n", nil),
	}
	r, err := Diagnose(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Controller != ControllerUnknown {
		t.Fatalf("controller = %q, want %q", r.Controller, ControllerUnknown)
	}
}

func TestMountHasDiscardReadFails(t *testing.T) {
	t.Parallel()
	opts := Options{ReadFileFn: func(string) ([]byte, error) { return nil, errRun }}
	if mountHasDiscard(opts, testDevice) {
		t.Fatal("expected false when /proc/mounts read fails")
	}
}

func TestDiscardLimitsParse(t *testing.T) {
	t.Parallel()
	opts := Options{RunFn: stubRun("", "", "sdd 1048576 4294966784\n", nil)}
	gran, max := discardLimits(context.Background(), opts, testDevice)
	if gran != 1048576 || max != 4294966784 {
		t.Fatalf("gran=%d max=%d, want 1048576/4294966784", gran, max)
	}
}

func TestDiscardLimitsRunFails(t *testing.T) {
	t.Parallel()
	opts := Options{RunFn: stubRun("", "", "", map[string]error{"lsblk": errRun})}
	gran, max := discardLimits(context.Background(), opts, testDevice)
	if gran != 0 || max != 0 {
		t.Fatalf("gran=%d max=%d, want 0/0 on lsblk failure", gran, max)
	}
}

func TestHostHeadroomViolation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		headroomGB int64
		metricPath string
		metricJSON string
		readErr    bool
		want       bool
	}{
		{name: "disabled when headroom zero", headroomGB: 0, metricPath: "/m.json", metricJSON: `{"v_free_gb":1}`, want: false},
		{name: "disabled when no path", headroomGB: 15, metricPath: "", metricJSON: `{"v_free_gb":1}`, want: false},
		{name: "violation below headroom", headroomGB: 15, metricPath: "/m.json", metricJSON: `{"v_free_gb":5}`, want: true},
		{name: "ok above headroom", headroomGB: 15, metricPath: "/m.json", metricJSON: `{"v_free_gb":50}`, want: false},
		{name: "read error → no violation", headroomGB: 15, metricPath: "/m.json", readErr: true, want: false},
		{name: "bad json → no violation", headroomGB: 15, metricPath: "/m.json", metricJSON: "{oops", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{
				HostHeadroomGB:  tt.headroomGB,
				HostMetricsPath: tt.metricPath,
				ReadFileFn: func(string) ([]byte, error) {
					if tt.readErr {
						return nil, errRun
					}
					return []byte(tt.metricJSON), nil
				},
			}
			if got := hostHeadroomViolation(opts); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDiagnoseHostHeadroomViolationFlagged(t *testing.T) {
	t.Parallel()
	opts := Options{
		RootPath:        "/",
		HostHeadroomGB:  15,
		HostMetricsPath: "/host-metrics.json",
		ReadFileFn: func(path string) ([]byte, error) {
			if path == procMountsPath {
				return []byte(procDiscard), nil
			}
			if path == "/host-metrics.json" {
				return []byte(`{"v_free_gb":3}`), nil
			}
			return nil, errors.New("not found")
		},
		RunFn: stubRun(findmntJSON, "scsi\n", "sdd 1048576 4294966784\n", nil),
	}
	r, err := Diagnose(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.HostHeadroomViolation {
		t.Fatal("expected host_headroom_violation true at 3GB free vs 15GB headroom")
	}
}

func TestDiagnoseReferenceTestDelta(t *testing.T) {
	t.Parallel()
	opts := Options{
		RootPath:      "/",
		ReferenceTest: true,
		ReadFileFn:    func(string) ([]byte, error) { return []byte(procDiscard), nil },
		RunFn: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "findmnt":
				return []byte(findmntJSON), nil
			case "lsblk":
				if containsArg(args, "TRAN") {
					return []byte("scsi\n"), nil
				}
				return []byte("sdd 1048576 4294966784\n"), nil
			case "civm-reference-test":
				return []byte("104857600\n"), nil
			}
			return nil, nil
		},
	}
	r, err := Diagnose(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ReferenceDeltaBytes != 104857600 {
		t.Fatalf("reference_delta = %d, want 104857600", r.ReferenceDeltaBytes)
	}
}

func TestDiagnoseAppliesDefaults(t *testing.T) {
	t.Parallel()
	// Empty RootPath defaults to "/"; nil fns are replaced (we still inject
	// findmnt via RunFn so no real exec runs by overriding after defaults).
	opts := Options{
		ReadFileFn: func(string) ([]byte, error) { return []byte(procDiscard), nil },
		RunFn:      stubRun(findmntJSON, "scsi\n", "sdd 1048576 4294966784\n", nil),
	}
	r, err := Diagnose(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Device != testDevice {
		t.Fatalf("device = %q, want %q with default RootPath", r.Device, testDevice)
	}
}

func TestDefaultOptionsWired(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	if opts.RootPath != "/" {
		t.Fatalf("RootPath = %q, want /", opts.RootPath)
	}
	if opts.ReadFileFn == nil || opts.RunFn == nil {
		t.Fatal("DefaultOptions must wire ReadFileFn and RunFn")
	}
}

func TestRenderJSONRoundtrip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Report{Device: testDevice, Controller: ControllerSCSI, MountDiscard: true,
		DiscMaxBytes: 4294966784, TrimEffective: true, RootCause: rootCauseTrimSupported}
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatal(err)
	}
	var parsed Report
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v", err)
	}
	if parsed.Device != testDevice || !parsed.TrimEffective || parsed.RootCause != rootCauseTrimSupported {
		t.Fatalf("roundtrip mismatch: %+v", parsed)
	}
}

func TestRenderJSONOmitsHeadroomWhenFalse(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderJSON(&buf, Report{Device: testDevice}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "host_headroom_violation") {
		t.Fatalf("expected host_headroom_violation omitted, got %q", buf.String())
	}
}

func TestRenderTextIncludesFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	RenderText(&buf, Report{Device: testDevice, Controller: ControllerIDE,
		RootCause: rootCauseIDEController, HostHeadroomViolation: true, ReferenceDeltaBytes: 100})
	out := buf.String()
	for _, want := range []string{testDevice, ControllerIDE, rootCauseIDEController, "host_headroom_violation", "reference_delta"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text missing %q in %q", want, out)
		}
	}
}

func TestDeviceBaseName(t *testing.T) {
	t.Parallel()
	if got := deviceBaseName("/dev/sdd"); got != testBase {
		t.Fatalf("base = %q, want %q", got, testBase)
	}
}

func TestParseInt64Invalid(t *testing.T) {
	t.Parallel()
	if got := parseInt64("not-a-number"); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}
