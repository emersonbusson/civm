package hostdisk

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

const (
	testNow      = "2026-05-29T12:00:00Z"
	testFresh    = "2026-05-29T11:55:00Z"
	testStaleTS  = "2026-05-29T11:00:00Z"
	testBadTS    = "not-a-timestamp"
	testPath     = "/var/lib/civm/host-metrics.json"
	deliveryFail = "failed"
)

func fixedNow(t *testing.T) func() time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, testNow)
	if err != nil {
		t.Fatalf("parse testNow: %v", err)
	}
	return func() time.Time { return now }
}

func optsWith(t *testing.T, data []byte, readErr error) Options {
	t.Helper()
	opts := DefaultOptions()
	opts.NowFn = fixedNow(t)
	opts.ReadFileFn = func(string) ([]byte, error) { return data, readErr }
	return opts
}

func marshalMetrics(t *testing.T, m Metrics) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}
	return b
}

func TestCheckLevelOK(t *testing.T) {
	m := Metrics{VFreeGB: 50, VSizeGB: 200, VHDXMaxSizeGB: 150, VHDXBlockSizeBytes: 1048576, Timestamp: testFresh}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Level != levelOK || r.Stale {
		t.Fatalf("want ok/not-stale, got %+v", r)
	}
	if r.FreeHeadroomViolation || r.AllocationHeadroomViolation {
		t.Fatalf("want no headroom violations, got %+v", r)
	}
	if r.VHDXBlockSizeBytes != 1048576 {
		t.Fatalf("block size = %d, want 1048576", r.VHDXBlockSizeBytes)
	}
}

func TestCheckLevelWarn(t *testing.T) {
	// VFreeGB between crit (10) and warn (30): 25 -> warn.
	m := Metrics{VFreeGB: 25, VSizeGB: 200, VHDXMaxSizeGB: 150, Timestamp: testFresh}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Level != levelWarn {
		t.Fatalf("want warn, got %+v", r)
	}
	if r.Reason == "" {
		t.Fatalf("warn level must carry a reason: %+v", r)
	}
}

func TestCheckLevelCrit(t *testing.T) {
	m := Metrics{VFreeGB: 8, VSizeGB: 200, VHDXMaxSizeGB: 150, Timestamp: testFresh}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Level != levelCrit {
		t.Fatalf("want crit, got %+v", r)
	}
}

func TestCheckStaleForcesCrit(t *testing.T) {
	// VFreeGB looks healthy (50) but the snapshot is older than MaxAge.
	m := Metrics{VFreeGB: 50, VSizeGB: 200, VHDXMaxSizeGB: 150, Timestamp: testStaleTS}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.Stale || r.Level != levelCrit {
		t.Fatalf("stale snapshot with healthy free must be crit, got %+v", r)
	}
	if r.Reason != reasonStale {
		t.Fatalf("want stale reason, got %q", r.Reason)
	}
}

func TestCheckDeliveryFailedForcesCrit(t *testing.T) {
	// VFreeGB healthy but delivery_status=failed -> crit, not stale.
	m := Metrics{VFreeGB: 50, VSizeGB: 200, VHDXMaxSizeGB: 150, Timestamp: testFresh, DeliveryStatus: deliveryFail}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Level != levelCrit {
		t.Fatalf("delivery failed must be crit, got %+v", r)
	}
	if r.Stale {
		t.Fatalf("fresh-but-failed must not be marked stale: %+v", r)
	}
	if r.Reason != reasonDeliveryFailed {
		t.Fatalf("want delivery-failed reason, got %q", r.Reason)
	}
}

func TestCheckFreeHeadroomViolation(t *testing.T) {
	// VFreeGB below headroom (8) but allocation headroom satisfied.
	m := Metrics{VFreeGB: 6, VSizeGB: 300, VHDXMaxSizeGB: 150, Timestamp: testFresh}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.FreeHeadroomViolation {
		t.Fatalf("want free headroom violation, got %+v", r)
	}
	if r.AllocationHeadroomViolation {
		t.Fatalf("allocation headroom should be fine (300-150=150>=8): %+v", r)
	}
}

func TestCheckAllocationHeadroomViolation(t *testing.T) {
	// VSizeGB-VHDXMaxSizeGB = 155-150 = 5 < 8 -> allocation violation.
	// VFreeGB high (40) so no free headroom violation.
	m := Metrics{VFreeGB: 40, VSizeGB: 155, VHDXMaxSizeGB: 150, Timestamp: testFresh}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.AllocationHeadroomViolation {
		t.Fatalf("want allocation headroom violation, got %+v", r)
	}
	if r.FreeHeadroomViolation {
		t.Fatalf("free headroom should be fine (40>=8): %+v", r)
	}
}

func TestCheckFileAbsentIsStale(t *testing.T) {
	opts := optsWith(t, nil, &fs.PathError{Op: "open", Path: testPath, Err: fs.ErrNotExist})
	r, err := Check(opts)
	if err != nil {
		t.Fatalf("absent file must not be a hard error, got %v", err)
	}
	if !r.Stale || r.Level != levelCrit {
		t.Fatalf("absent file must be stale+crit, got %+v", r)
	}
	if r.Reason != reasonFileAbsent {
		t.Fatalf("want file-absent reason, got %q", r.Reason)
	}
}

func TestCheckReadErrorReturnsErr(t *testing.T) {
	opts := optsWith(t, nil, errors.New("EIO"))
	r, err := Check(opts)
	if err == nil {
		t.Fatalf("non-notexist read error must surface as error")
	}
	if !r.Stale || r.Level != levelCrit {
		t.Fatalf("read error must still report stale+crit, got %+v", r)
	}
}

func TestCheckParseErrorReturnsErr(t *testing.T) {
	opts := optsWith(t, []byte("{not json"), nil)
	r, err := Check(opts)
	if err == nil {
		t.Fatalf("invalid json must surface as error")
	}
	if !r.Stale || r.Level != levelCrit {
		t.Fatalf("parse error must report stale+crit, got %+v", r)
	}
}

func TestCheckMissingTimestampIsStale(t *testing.T) {
	m := Metrics{VFreeGB: 50, VSizeGB: 200, VHDXMaxSizeGB: 150}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.Stale || r.Reason != reasonMissingTime {
		t.Fatalf("missing timestamp must be stale, got %+v", r)
	}
}

func TestCheckBadTimestampIsStale(t *testing.T) {
	m := Metrics{VFreeGB: 50, VSizeGB: 200, VHDXMaxSizeGB: 150, Timestamp: testBadTS}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.Stale || r.Reason != reasonTimestampParse {
		t.Fatalf("bad timestamp must be stale, got %+v", r)
	}
}

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()
	if opts.Path != testPath {
		t.Fatalf("default path = %q", opts.Path)
	}
	if opts.MaxAge != 30*time.Minute {
		t.Fatalf("default maxage = %v", opts.MaxAge)
	}
	if opts.WarnFreeGB != 30 || opts.CritFreeGB != 10 || opts.HeadroomGB != 8 {
		t.Fatalf("default thresholds = %+v", opts)
	}
}

func TestRenderJSON(t *testing.T) {
	m := Metrics{VFreeGB: 50, VSizeGB: 200, VHDXMaxSizeGB: 150, Timestamp: testFresh}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var buf bytes.Buffer
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatalf("render json: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("roundtrip json: %v", err)
	}
	if decoded.Level != levelOK || decoded.VFreeGB != 50 {
		t.Fatalf("roundtrip mismatch: %+v", decoded)
	}
	if !strings.Contains(buf.String(), "v_free_gb") {
		t.Fatalf("json must use snake_case field names: %s", buf.String())
	}
}

func TestRenderText(t *testing.T) {
	m := Metrics{VFreeGB: 6, VSizeGB: 155, VHDXMaxSizeGB: 150, VHDXBlockSizeBytes: 1048576, Timestamp: testFresh}
	r, err := Check(optsWith(t, marshalMetrics(t, m), nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var buf bytes.Buffer
	RenderText(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "Host disk:") {
		t.Fatalf("text render missing header: %s", out)
	}
	if !strings.Contains(out, "FreeHeadroomViolation") || !strings.Contains(out, "AllocationHeadroomViolation") {
		t.Fatalf("text render must surface both headroom violations: %s", out)
	}
	if !strings.Contains(out, "VHDXBlockSizeBytes: 1048576") {
		t.Fatalf("text render must surface VHDX block size: %s", out)
	}
}

// TestReportWantsCleanupAndBlocks valida a semântica host-aware do gate
// job-started: warn/crit pedem cleanup; só crit FRESCO bloqueia o job. crit por
// staleness (telemetria ausente) NÃO bloqueia — não auto-sabotar a CI.
func TestReportWantsCleanupAndBlocks(t *testing.T) {
	tests := []struct {
		name        string
		level       string
		stale       bool
		wantCleanup bool
		wantBlocks  bool
	}{
		{"ok host: no cleanup, no block", levelOK, false, false, false},
		{"warn host: cleanup, no block", levelWarn, false, true, false},
		{"fresh crit host: cleanup AND block", levelCrit, false, true, true},
		{"stale crit (metrics absent): cleanup but NO block", levelCrit, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Report{Level: tt.level, Stale: tt.stale}
			if got := r.WantsCleanup(); got != tt.wantCleanup {
				t.Errorf("WantsCleanup()=%v, want %v", got, tt.wantCleanup)
			}
			if got := r.Blocks(); got != tt.wantBlocks {
				t.Errorf("Blocks()=%v, want %v", got, tt.wantBlocks)
			}
		})
	}
}
