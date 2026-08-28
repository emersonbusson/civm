package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/cireport"
)

func runCI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "uso: civmctl ci <local-report> [flags]")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "local-report":
		return runCILocalReport(rest)
	default:
		fmt.Fprintf(os.Stderr, "subcomando ci desconhecido: %s\n", sub)
		return exitUsage
	}
}

func runCILocalReport(args []string) int {
	fs := flag.NewFlagSet("ci local-report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "owner/repo (ex: owner/repo)")
	sha := fs.String("sha", "", "commit SHA (full 40-char)")
	state := fs.String("state", "", "success | failure | pending | error")
	checkContext := fs.String("context", "civm fallback", "check context (ex: 'civm fallback')")
	description := fs.String("description", "", "descricao curta (<=140 chars)")
	targetURL := fs.String("target-url", "", "URL para detalhes (opcional)")
	timeoutSec := fs.Int("timeout", 15, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de ci local-report:", err)
		return exitUsage
	}
	opts := cireport.DefaultOptions()
	opts.Repo = *repo
	opts.SHA = *sha
	opts.State = cireport.State(*state)
	opts.Context = *checkContext
	opts.Description = *description
	opts.TargetURL = *targetURL
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	resp, err := cireport.Post(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		if len(resp) > 0 {
			fmt.Fprintln(os.Stderr, "response:", string(resp))
		}
		return 1
	}
	cireport.Render(opts, resp, os.Stdout)
	return 0
}
