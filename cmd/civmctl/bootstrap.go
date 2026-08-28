package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/bootstrap"
)

func runBootstrap(args []string) int {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	execute := fs.Bool("execute", false, "aplicar mudancas (default: dry-run)")
	dryRun := fs.Bool("dry-run", false, "explicito dry-run (default ja e dry-run)")
	watchdog := fs.Bool("watchdog", true, "instala civmctl-disk-watchdog.timer (hourly) alem do cleanup.timer")
	runnerWatchdog := fs.Bool("runner-watchdog", true, "instala civmctl-runner-watchdog.timer (auto-repair de runner)")
	reverseWatchdog := fs.Bool("reverse-watchdog", true, "instala civmctl-reverse-watchdog.timer (alarm-of-alarm)")
	metricsTimer := fs.Bool("metrics-timer", true, "instala civmctl-metrics.timer (Prometheus textfile)")
	timeoutMin := fs.Int("timeout", 30, "timeout em minutos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de bootstrap:", err)
		return exitUsage
	}
	if *execute && *dryRun {
		fmt.Fprintln(os.Stderr, "erro: --execute e --dry-run sao mutuamente exclusivos")
		return exitUsage
	}
	if *execute {
		fmt.Fprintln(os.Stderr, "AVISO: --execute vai modificar o sistema (apt install, /usr/local/go, etc).")
		fmt.Fprintln(os.Stderr, "Pressione Ctrl+C em 5s para abortar...")
		time.Sleep(5 * time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	opts := bootstrap.DefaultOptions()
	opts.Execute = *execute
	opts.WatchdogTimer = *watchdog
	opts.RunnerWatchdog = *runnerWatchdog
	opts.ReverseWatchdog = *reverseWatchdog
	opts.MetricsTimer = *metricsTimer
	results := bootstrap.Run(ctx, opts)
	bootstrap.RenderTable(results, opts, os.Stdout)
	for _, r := range results {
		if r.Err != nil {
			return 1
		}
	}
	return 0
}
