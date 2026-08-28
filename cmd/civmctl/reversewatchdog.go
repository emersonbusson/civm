package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/reversewatchdog"
)

func runReverseWatchdog(args []string) int {
	fs := flag.NewFlagSet("reverse-watchdog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	unit := fs.String("unit", "civmctl-disk-watchdog.service", "unit a monitorar")
	maxAgeHours := fs.Int("max-age-hours", civm.DefaultReverseMaxAgeHours, "alerta se ultima execucao >MaxAge")
	timeoutSec := fs.Int("timeout", 10, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de reverse-watchdog:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	opts := reversewatchdog.DefaultOptions()
	opts.Unit = *unit
	opts.MaxAge = time.Duration(*maxAgeHours) * time.Hour
	r := reversewatchdog.Check(ctx, opts)
	r.Render(os.Stdout)
	return r.Status.ExitCode()
}
