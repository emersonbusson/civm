package memwatchdog

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mk builds a /proc/meminfo-like blob from kB values.
func mk(memTotalKB, memAvailKB, swapTotalKB, swapFreeKB int64) string {
	return fmt.Sprintf(
		"MemTotal:       %d kB\nMemFree:         123456 kB\nMemAvailable:    %d kB\nBuffers:           1000 kB\nSwapTotal:       %d kB\nSwapFree:        %d kB\n",
		memTotalKB, memAvailKB, swapTotalKB, swapFreeKB)
}

func checkWith(t *testing.T, meminfo string) Result {
	t.Helper()
	return Check(context.Background(), Options{
		MeminfoFn: func() (string, error) { return meminfo, nil },
	})
}

func TestClassify(t *testing.T) {
	const total = 10_000_000 // kB
	const swapTotal = 2_097_152
	tests := []struct {
		name     string
		availKB  int64
		swapUsed int64 // kB
		wantDec  Decision
		wantExit int
	}{
		{"healthy", 8_200_000, 0, DecisionOK, 0},                        // 82% avail, no swap
		{"warn low avail", 1_200_000, 0, DecisionWarn, 1},               // 12% (between 8 and 15)
		{"warn swap", 5_000_000, 600 * 1024, DecisionWarn, 1},           // 50% avail but swap 600MB
		{"critical low avail", 500_000, 0, DecisionCritical, 2},         // 5% (<8)
		{"critical swap", 5_000_000, 2_000 * 1024, DecisionCritical, 2}, // swap ~2000MB (>1536)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			swapFree := swapTotal - tt.swapUsed
			res := checkWith(t, mk(total, tt.availKB, swapTotal, swapFree))
			if res.Decision != tt.wantDec {
				t.Errorf("decision = %s, want %s (reason=%q, mem=%+v)", res.Decision, tt.wantDec, res.Reason, res.Mem)
			}
			if res.Decision.ExitCode() != tt.wantExit {
				t.Errorf("exit = %d, want %d", res.Decision.ExitCode(), tt.wantExit)
			}
		})
	}
}

func TestParseMeminfoNormalizes(t *testing.T) {
	res := checkWith(t, mk(10_485_760 /*10GiB*/, 8_388_608 /*8GiB*/, 2_097_152 /*2GiB*/, 1_048_576 /*1GiB free*/))
	m := res.Mem
	if m.MemTotalMB != 10240 {
		t.Errorf("MemTotalMB = %d, want 10240", m.MemTotalMB)
	}
	if m.MemAvailableMB != 8192 {
		t.Errorf("MemAvailableMB = %d, want 8192", m.MemAvailableMB)
	}
	if m.AvailPct != 80 {
		t.Errorf("AvailPct = %d, want 80", m.AvailPct)
	}
	if m.SwapUsedMB != 1024 { // 2GiB total - 1GiB free = 1GiB used
		t.Errorf("SwapUsedMB = %d, want 1024", m.SwapUsedMB)
	}
}

func TestSwapUsedNeverNegative(t *testing.T) {
	// SwapFree > SwapTotal (shouldn't happen, but be robust) → used clamps to 0.
	res := checkWith(t, mk(10_000_000, 8_000_000, 1_000_000, 1_200_000))
	if res.Mem.SwapUsedMB != 0 {
		t.Errorf("SwapUsedMB = %d, want 0 (clamped)", res.Mem.SwapUsedMB)
	}
}

func TestMeminfoReadErrorIsCritical(t *testing.T) {
	res := Check(context.Background(), Options{
		MeminfoFn: func() (string, error) { return "", fmt.Errorf("boom") },
	})
	if res.Decision != DecisionCritical || res.Reason != "meminfo-read-failed" {
		t.Fatalf("got decision=%s reason=%q, want critical/meminfo-read-failed", res.Decision, res.Reason)
	}
}

func TestMeminfoParseErrorIsCritical(t *testing.T) {
	res := checkWith(t, "garbage without the fields\n")
	if res.Decision != DecisionCritical || res.Reason != "meminfo-parse-failed" {
		t.Fatalf("got decision=%s reason=%q, want critical/meminfo-parse-failed", res.Decision, res.Reason)
	}
}

func TestSampleReturnsParsedMeminfo(t *testing.T) {
	mem, err := Sample(Options{
		MeminfoFn: func() (string, error) {
			return mk(10_485_760 /*10GiB*/, 8_388_608 /*8GiB*/, 2_097_152 /*2GiB*/, 1_048_576 /*1GiB free*/), nil
		},
	})
	if err != nil {
		t.Fatalf("Sample err = %v", err)
	}
	if mem.MemTotalMB != 10240 {
		t.Errorf("MemTotalMB = %d, want 10240", mem.MemTotalMB)
	}
	if mem.MemAvailableMB != 8192 {
		t.Errorf("MemAvailableMB = %d, want 8192", mem.MemAvailableMB)
	}
	if mem.SwapUsedMB != 1024 {
		t.Errorf("SwapUsedMB = %d, want 1024", mem.SwapUsedMB)
	}
}

func TestSamplePropagatesReadError(t *testing.T) {
	_, err := Sample(Options{
		MeminfoFn: func() (string, error) { return "", fmt.Errorf("boom") },
	})
	if err == nil {
		t.Fatalf("Sample err = nil, want propagated read error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Sample err = %v, want wrapped boom", err)
	}
}

func TestSampleParseErrorPropagates(t *testing.T) {
	_, err := Sample(Options{
		MeminfoFn: func() (string, error) { return "garbage without fields\n", nil },
	})
	if err == nil {
		t.Fatalf("Sample err = nil, want parse error")
	}
}

func TestRenderAndJSON(t *testing.T) {
	res := checkWith(t, mk(10_000_000, 8_200_000, 2_097_152, 2_097_152))
	var human strings.Builder
	res.Render(&human)
	if !strings.Contains(human.String(), "mem-watchdog") || !strings.Contains(human.String(), "MemAvailable") {
		t.Errorf("human render missing fields: %s", human.String())
	}
	var js strings.Builder
	if err := res.RenderJSON(&js); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !strings.Contains(js.String(), "\"status\":\"ok\"") || !strings.Contains(js.String(), "mem_available_mb") {
		t.Errorf("json missing expected keys: %s", js.String())
	}
}
