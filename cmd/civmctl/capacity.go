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
)

func runCapacity(args []string) int {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON para Busson/outros integradores")
	path := fs.String("path", "/", "filesystem a medir")
	maxDiskPct := fs.Int("max-disk-pct", civm.DefaultCapacityMaxDiskPct, "accepting_jobs=false se disco usado >= pct")
	timeoutSec := fs.Int("timeout", civm.DefaultHealthTimeoutSeconds, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de capacity:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	opts := capacity.DefaultOptions()
	opts.Path = *path
	opts.MaxDiskPct = *maxDiskPct
	r := capacity.Check(ctx, opts)
	if *jsonOut {
		if err := capacity.RenderJSON(os.Stdout, r); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		capacity.RenderText(os.Stdout, r)
	}
	if !r.AcceptingJobs {
		return 1
	}
	return 0
}
