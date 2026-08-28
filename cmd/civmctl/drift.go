package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/drift"
)

func runDrift(args []string) int {
	fs := flag.NewFlagSet("drift", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeoutSec := fs.Int("timeout", 15, "timeout em segundos para fetch upstream")
	urlFlag := fs.String("url", "", "URL upstream alternativa (default: spec.UpstreamURL)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de drift:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	opts := drift.DefaultOptions()
	if *urlFlag != "" {
		opts.UpstreamURL = *urlFlag
	}
	report, err := drift.Detect(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro detectando drift:", err)
		return 2
	}
	report.Render(os.Stdout)
	if report.HasBehind() {
		return 1
	}
	return 0
}
