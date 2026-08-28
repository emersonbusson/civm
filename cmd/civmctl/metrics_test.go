package main

import (
	"testing"

	"github.com/emersonbusson/civm/internal/capacity"
	"github.com/emersonbusson/civm/internal/memwatchdog"
)

// TestBuildSamplesIncludesMemoryGauges prova que o dump de metricas expoe a
// pressao de memoria (CIVM-4 / ADR-107): ate aqui o admit gateava jobs heavy por
// RAM, mas a pressao era invisivel no Prometheus. Sem isto nao ha como
// correlacionar OOM/thrash com timeout de bring-up.
func TestBuildSamplesIncludesMemoryGauges(t *testing.T) {
	r := capacity.Report{AcceptingJobs: false, QueueStalled: true}
	mem := memwatchdog.Result{
		Decision: memwatchdog.DecisionWarn,
		Mem: memwatchdog.Meminfo{
			MemTotalMB:     7800,
			MemAvailableMB: 900,
			AvailPct:       11,
			SwapUsedMB:     256,
		},
	}

	byName := map[string]float64{}
	for _, s := range buildSamples(r, mem) {
		byName[s.Name] = s.Value
	}

	want := map[string]float64{
		"civm_mem_total_mb":         7800,
		"civm_mem_available_mb":     900,
		"civm_mem_available_pct":    11,
		"civm_swap_used_mb":         256,
		"civm_mem_pressure":         1, // DecisionWarn
		"civm_runner_queue_stalled": 1,
	}
	for name, w := range want {
		got, ok := byName[name]
		if !ok {
			t.Fatalf("gauge %q ausente nos samples", name)
		}
		if got != w {
			t.Fatalf("%s = %v, want %v", name, got, w)
		}
	}
}
