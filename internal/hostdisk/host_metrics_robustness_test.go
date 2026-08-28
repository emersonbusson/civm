package hostdisk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostMetricsVhdReadIsTimeoutGuarded protege contra o hang que cegou a
// barreira host-aware em 2026-06-18.
//
// O `civm-host-metrics.ps1` lê o tamanho do VHDX com `Get-VHD`. Esse cmdlet
// PENDURA quando o `Optimize-VHD` do orchestrator está compactando o mesmo VHDX
// (os dois donos host-side disputam o lock do disco). A leitura nua, sem
// timeout, travou a task de host-metrics por 6h; sem snapshot fresco, o gate
// `job-started` host-aware leu um valor stale (`v_free=43` enquanto o V: real
// estava em 16). Stale é tratado como "não bloqueia" (DT-v2-5, fail-open) — o
// gate admitiu o job, o disco furou, e o `panic_compact` do orchestrator matou
// os jobs `service-billing` e `tenant-isolation-smoke`.
//
// A leitura do VHDX DEVE rodar sob um timeout (job + `Wait-Job -Timeout`): se o
// disco está locked, ela retorna null e o snapshot sai com o `v_free` crítico
// (de `Get-Volume`, que não trava) + campos vhdx nulos, em vez de pendurar o
// script inteiro e cegar a barreira. O `v_free` é o único campo que o gate
// precisa; o tamanho do VHDX é só observability.
func TestHostMetricsVhdReadIsTimeoutGuarded(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "windows", "civm-host-metrics.ps1")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read civm-host-metrics.ps1: %v", err)
	}
	src := string(data)

	if !strings.Contains(src, "Wait-Job") || !strings.Contains(src, "-Timeout") {
		t.Error("civm-host-metrics.ps1 must read the VHDX under a Wait-Job -Timeout guard; " +
			"a bare Get-VHD hangs when Optimize-VHD holds the VHDX lock and blinds the host-aware gate")
	}
	if strings.Contains(src, "Get-VHD -Path $resolvedVhdx -ErrorAction Stop") {
		t.Error("civm-host-metrics.ps1 still has the bare hanging 'Get-VHD -Path $resolvedVhdx -ErrorAction Stop' " +
			"in the main flow; the VHDX read must go through the timeout guard")
	}
}

// TestHostMetricsTaskHasExecutionTimeLimit é a segunda camada (defense in depth)
// do mesmo incidente. Mesmo com o guard de timeout no script, se um `Get-VHD`
// escapar e pendurar, a scheduled task DEVE ter `ExecutionTimeLimit` para matar
// a instância presa. Sem isso, um hang bloqueou os runs de 10-em-10min por 6h —
// porque a task não tinha como encerrar a instância travada. O limite força um
// teto de tempo: instância presa morre, o próximo run produz snapshot fresco, e
// a barreira volta a enxergar.
func TestHostMetricsTaskHasExecutionTimeLimit(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "windows", "register-civm-host-metrics.ps1")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read register-civm-host-metrics.ps1: %v", err)
	}
	if !strings.Contains(string(data), "ExecutionTimeLimit") {
		t.Error("register-civm-host-metrics.ps1 must set ExecutionTimeLimit so a hung instance " +
			"(Get-VHD on a locked VHDX) is killed instead of blocking the 10-min runs")
	}
}
