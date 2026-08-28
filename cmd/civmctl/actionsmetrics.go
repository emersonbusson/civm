package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/actionsmetrics"
)

func runActionsMetrics(args []string) int {
	fs := flag.NewFlagSet("actions-metrics", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	org := fs.String("org", "", "GitHub organization (obrigatório)")
	period := fs.String("period", "month", "month | last-month | week | day | YYYY-MM-DD..YYYY-MM-DD")
	reposRaw := fs.String("repos", "auto", "auto | none | owner/repo separados por virgula")
	concurrency := fs.Int("concurrency", 4, "máximo de chamadas gh paralelas")
	jsonOut := fs.Bool("json", false, "saída JSON estruturada")
	timeoutSec := fs.Int("timeout", 30, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de actions-metrics:", err)
		return exitUsage
	}
	if strings.TrimSpace(*org) == "" {
		fmt.Fprintln(os.Stderr, "erro: --org obrigatório (ex: --org=acme)")
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	start, end, err := actionsmetrics.Period(*period, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return exitUsage
	}

	opts := actionsmetrics.DefaultOptions()
	opts.Organization = *org
	opts.StartDate = start
	opts.EndDate = end
	opts.Concurrency = *concurrency
	if err := configureActionsMetricsRepos(*reposRaw, &opts); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return exitUsage
	}

	report, err := actionsmetrics.Collect(ctx, opts)
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

func configureActionsMetricsRepos(raw string, opts *actionsmetrics.Options) error {
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
