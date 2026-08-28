package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/health"
)

func runHealth(args []string) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workDir := fs.String("work-dir", civm.DefaultHealthDiskPath, "path/filesystem para checar disco")
	jsonOut := fs.Bool("json", false, "saida JSON estruturada (pra Prometheus/dashboards)")
	timeoutSec := fs.Int("timeout", civm.DefaultHealthTimeoutSeconds, "timeout em segundos para coleta")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de health:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	c := health.NewDefaultCollector(*workDir)
	r := c.Collect(ctx)
	if *jsonOut {
		if err := r.RenderJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		r.Render(os.Stdout)
	}
	return r.Exit()
}
