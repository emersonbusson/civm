package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/activeruns"
)

func runActiveRuns(args []string) int {
	fs := flag.NewFlagSet("active-runs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reposRaw := fs.String("repos", "auto", "repos: auto (descobre via systemd), none, ou owner/repo separados por virgula")
	includeETA := fs.Bool("include-eta", true, "estimar avg_duration_sec por workflow (1 chamada gh extra por workflow ativo)")
	limit := fs.Int("limit", 5, "máximo de runs por (repo, status)")
	historyLimit := fs.Int("history-limit", 10, "máximo de runs success usadas pra estimar ETA por workflow")
	concurrency := fs.Int("concurrency", 8, "número máximo de chamadas gh paralelas")
	jsonOut := fs.Bool("json", false, "saída JSON estruturada")
	timeoutSec := fs.Int("timeout", 30, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de active-runs:", err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	opts := activeruns.DefaultOptions()
	opts.IncludeETA = *includeETA
	opts.Limit = *limit
	opts.HistoryLimit = *historyLimit
	opts.Concurrency = *concurrency
	if err := configureActiveRunsRepos(*reposRaw, &opts); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de active-runs:", err)
		return exitUsage
	}

	report, err := activeruns.Collect(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return exitUsage
	}
	if *jsonOut {
		if err := report.RenderJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		report.Render(os.Stdout)
	}
	return report.Exit
}

func configureActiveRunsRepos(raw string, opts *activeruns.Options) error {
	mode := strings.TrimSpace(raw)
	switch mode {
	case "auto":
		opts.InferRepos = true
		opts.Repos = nil
	case "", "none":
		opts.InferRepos = false
		opts.Repos = nil
	default:
		opts.InferRepos = false
		opts.Repos = splitCSV(raw)
	}
	return nil
}
