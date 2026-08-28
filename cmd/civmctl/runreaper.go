package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/runreaper"
)

func runReapRuns(args []string) int {
	fs := flag.NewFlagSet("reap-runs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reposRaw := fs.String("repos", "", "repos owner/repo separados por virgula (obrigatorio)")
	execute := fs.Bool("execute", false, "cancela de fato (default dry-run)")
	maxCancel := fs.Int("max-cancel", runreaper.DefaultMaxCancelPerRepo, "cap de cancelamentos por repo por execucao")
	jsonOut := fs.Bool("json", false, "saida JSON")
	timeout := fs.Duration("timeout", 120*time.Second, "timeout total")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de reap-runs:", err)
		return exitUsage
	}
	repos := splitCSV(*reposRaw)
	if len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "erro nos args de reap-runs: --repos deve informar pelo menos um owner/repo")
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	opts := runreaper.Options{
		Repos:            repos,
		Execute:          *execute,
		MaxCancelPerRepo: *maxCancel,
	}
	report := runreaper.Reap(ctx, opts)
	if *jsonOut {
		if err := report.RenderJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao serializar:", err)
			return 2
		}
	} else {
		report.Render(os.Stdout)
	}
	return report.Exit
}
