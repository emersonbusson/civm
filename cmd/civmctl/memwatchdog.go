package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/memwatchdog"
)

func runMemWatchdog(args []string) int {
	fs := flag.NewFlagSet("mem-watchdog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON (registro por tick no journal)")
	warnPct := fs.Int("warn-pct", 0, "warn se MemAvailable < N%% do total (default 15)")
	critPct := fs.Int("crit-pct", 0, "critical se MemAvailable < N%% do total (default 8)")
	warnSwap := fs.Int64("warn-swap-mb", 0, "warn se swap em uso > N MB (default 512)")
	critSwap := fs.Int64("crit-swap-mb", 0, "critical se swap em uso > N MB (default 1536)")
	timeoutSec := fs.Int("timeout", 5, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de mem-watchdog:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	opts := memwatchdog.DefaultOptions()
	if *warnPct > 0 {
		opts.WarnAvailPct = *warnPct
	}
	if *critPct > 0 {
		opts.CritAvailPct = *critPct
	}
	if *warnSwap > 0 {
		opts.WarnSwapMB = *warnSwap
	}
	if *critSwap > 0 {
		opts.CritSwapMB = *critSwap
	}

	res := memwatchdog.Check(ctx, opts)
	if *jsonOut {
		if err := res.RenderJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao serializar:", err)
			return 2
		}
	} else {
		res.Render(os.Stdout)
	}
	return res.Decision.ExitCode()
}
