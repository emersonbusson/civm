package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/peerstatus"
)

func runPeerStatus(args []string) int {
	fs := flag.NewFlagSet("peer-status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "owner/repo (ex: owner/repo)")
	reposRaw := fs.String("repos", "", "repos owner/repo separados por virgula")
	workflow := fs.String("workflow", "ci.yml", "nome do workflow file")
	jsonOut := fs.Bool("json", false, "saida JSON")
	timeoutSec := fs.Int("timeout", 20, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de peer-status:", err)
		return exitUsage
	}
	if *repo != "" && *reposRaw != "" {
		fmt.Fprintln(os.Stderr, "erro: use --repo ou --repos, nao ambos")
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	if *reposRaw != "" {
		opts := peerstatus.DefaultFleetOptions()
		opts.Repos = splitCSV(*reposRaw)
		opts.WorkflowFile = *workflow
		report, err := peerstatus.CollectFleet(ctx, opts)
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

	opts := peerstatus.DefaultOptions()
	opts.Repo = *repo
	opts.WorkflowFile = *workflow
	s, err := peerstatus.Collect(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return exitUsage
	}
	if *jsonOut {
		if err := s.RenderJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		s.Render(os.Stdout)
	}
	return s.Severity().ExitCode()
}
