package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/diskwatchdog"
)

func runDiskWatchdog(args []string) int {
	fs := flag.NewFlagSet("disk-watchdog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("path", "/", "filesystem a monitorar")
	thresholdPct := fs.Int("threshold-pct", civm.DefaultWatchdogThresholdPct, "disparar cleanup se used%% > threshold")
	workDir := fs.String("work-dir", civm.DefaultWorkDir, "diretorio do runner")
	tmpDir := fs.String("tmp-dir", civm.DefaultTmpDir, "diretorio /tmp")
	execute := fs.Bool("execute", false, "aplicar cleanup (default: dry-run)")
	timeoutMin := fs.Int("timeout", civm.DefaultCleanupTimeoutMinutes, "timeout em minutos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de disk-watchdog:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	opts := diskwatchdog.DefaultOptions()
	opts.Path = *path
	opts.ThresholdPct = *thresholdPct
	opts.WorkDir = *workDir
	opts.TmpDir = *tmpDir
	opts.Execute = *execute
	r := diskwatchdog.Check(ctx, opts)
	r.Render(os.Stdout)
	return r.ExitCode()
}
