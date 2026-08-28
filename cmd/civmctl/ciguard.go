package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/emersonbusson/civm/internal/ciguard"
)

func runCIGuard(args []string) int {
	fs := flag.NewFlagSet("ci-guard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRoot := fs.String("repo-root", ".", "raiz do repo consumidor a auditar")
	mode := fs.String("mode", ciguard.ModeReport, "report|enforce")
	jsonOut := fs.Bool("json", false, "saida JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de ci-guard:", err)
		return exitUsage
	}
	if *mode != ciguard.ModeReport && *mode != ciguard.ModeEnforce {
		fmt.Fprintf(os.Stderr, "erro: --mode invalido %q (use report|enforce)\n", *mode)
		return exitUsage
	}

	opts := ciguard.DefaultOptions(*repoRoot)
	opts.Mode = *mode
	result, err := ciguard.Scan(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro ao escanear repo:", err)
		return 1
	}
	if *jsonOut {
		if err := ciguard.RenderJSON(os.Stdout, result); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		ciguard.RenderText(os.Stdout, result)
	}
	if result.Mode == ciguard.ModeEnforce && result.Violations > 0 {
		return 1
	}
	return 0
}
