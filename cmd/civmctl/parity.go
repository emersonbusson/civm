package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/parity"
	"github.com/emersonbusson/civm/internal/specs"
)

func runParity(args []string) int {
	fs := flag.NewFlagSet("parity", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON estruturada")
	timeoutSec := fs.Int("timeout", 5, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de parity:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	report := parity.Check(ctx, parity.Options{
		Spec:    specs.Ubuntu2404(),
		Timeout: time.Duration(*timeoutSec) * time.Second,
	})
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
