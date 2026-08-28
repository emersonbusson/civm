package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/diskaudit"
)

func runDiskAudit(args []string) int {
	fs := flag.NewFlagSet("disk-audit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON")
	home := fs.String("home", "", "home base para .cache/go/pkg/codespace (default: user atual)")
	limit := fs.Int("limit", 20, "maximo de entradas")
	noDocker := fs.Bool("no-docker", false, "nao consultar docker system df")
	noSystem := fs.Bool("no-system", false, "nao medir /var/log e /var/cache")
	timeoutSec := fs.Int("timeout", 120, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de disk-audit:", err)
		return exitUsage
	}
	if *limit < 1 {
		fmt.Fprintln(os.Stderr, "erro: --limit deve ser >= 1")
		return exitUsage
	}
	if *timeoutSec < 1 {
		fmt.Fprintln(os.Stderr, "erro: --timeout deve ser >= 1")
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	opts := diskaudit.DefaultOptions()
	if *home != "" {
		opts.HomeDir = *home
	}
	opts.Limit = *limit
	opts.IncludeDocker = !*noDocker
	opts.IncludeSystem = !*noSystem
	report := diskaudit.Collect(ctx, opts)
	if *jsonOut {
		if err := diskaudit.RenderJSON(os.Stdout, report); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
		return 0
	}
	diskaudit.RenderText(os.Stdout, report)
	return 0
}
