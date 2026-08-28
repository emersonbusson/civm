package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/capacity"
	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/memwatchdog"
	"github.com/emersonbusson/civm/internal/metrics"
)

const defaultMetricsPath = "/var/lib/node_exporter/textfile_collector/civm.prom"

func runMetrics(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "uso: civmctl metrics dump [--out=path]")
		return exitUsage
	}
	if args[0] == "dump" {
		return runMetricsDump(args[1:])
	}
	fmt.Fprintf(os.Stderr, "metrics: subcomando desconhecido %q\n", args[0])
	return exitUsage
}

func runMetricsDump(args []string) int {
	fs := flag.NewFlagSet("metrics dump", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", defaultMetricsPath, "destino do textfile prometheus")
	stdout := fs.Bool("stdout", false, "imprime no stdout em vez de gravar no arquivo (debug)")
	timeoutSec := fs.Int("timeout", civm.DefaultHealthTimeoutSeconds, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de metrics dump:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	r := capacity.Check(ctx, capacity.DefaultOptions())
	mem := memwatchdog.Check(ctx, memwatchdog.DefaultOptions())
	samples := buildSamples(r, mem)
	if *stdout {
		if err := metrics.Render(os.Stdout, samples); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao renderizar metrics:", err)
			return 2
		}
		return 0
	}
	if err := metrics.WriteTextfile(*out, samples); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao gravar metrics:", err)
		return 1
	}
	return 0
}

func buildSamples(r capacity.Report, mem memwatchdog.Result) []metrics.Metric {
	accepting := 0.0
	if r.AcceptingJobs {
		accepting = 1
	}
	queueStalled := 0.0
	if r.QueueStalled {
		queueStalled = 1
	}
	return []metrics.Metric{
		{Name: "civm_disk_used_pct", Help: "Percentual de disco utilizado no path monitorado", Type: metrics.TypeGauge, Value: float64(r.DiskUsedPct)},
		{Name: "civm_disk_free_gb", Help: "Disco livre em GB", Type: metrics.TypeGauge, Value: float64(r.DiskFreeGB)},
		{Name: "civm_disk_total_gb", Help: "Disco total em GB", Type: metrics.TypeGauge, Value: float64(r.DiskTotalGB)},
		{Name: "civm_runner_services_active", Help: "Quantidade de services actions.runner.*", Type: metrics.TypeGauge, Value: float64(r.RunnerServices)},
		{Name: "civm_runner_workers_active", Help: "Quantidade de workers Runner.Worker em execução", Type: metrics.TypeGauge, Value: float64(r.RunnerWorkers)},
		{Name: "civm_runner_queue_stalled", Help: "1 se o watchdog detectou fila civm sem consumo; 0 caso contrário", Type: metrics.TypeGauge, Value: queueStalled},
		{Name: "civm_accepting_jobs", Help: "1 se runner está aceitando jobs (disco abaixo do threshold); 0 caso contrário", Type: metrics.TypeGauge, Value: accepting},
		// Pressao de memoria (CIVM-4 / ADR-107): o admit ja gateia jobs heavy por
		// RAM, mas ate aqui a pressao era invisivel no Prometheus — sem como
		// correlacionar OOM/thrash com timeout de bring-up. Leitura de meminfo que
		// falha -> Decision critical + campos zerados (o proprio pressure=2 sinaliza).
		{Name: "civm_mem_total_mb", Help: "RAM total em MB", Type: metrics.TypeGauge, Value: float64(mem.Mem.MemTotalMB)},
		{Name: "civm_mem_available_mb", Help: "RAM disponivel (free + reclaimable) em MB", Type: metrics.TypeGauge, Value: float64(mem.Mem.MemAvailableMB)},
		{Name: "civm_mem_available_pct", Help: "RAM disponivel como percentual do total", Type: metrics.TypeGauge, Value: float64(mem.Mem.AvailPct)},
		{Name: "civm_swap_used_mb", Help: "Swap em uso em MB (thrash quando alto)", Type: metrics.TypeGauge, Value: float64(mem.Mem.SwapUsedMB)},
		{Name: "civm_mem_pressure", Help: "Pressao de memoria: 0=ok, 1=warn, 2=critical", Type: metrics.TypeGauge, Value: float64(mem.Decision.ExitCode())},
	}
}
