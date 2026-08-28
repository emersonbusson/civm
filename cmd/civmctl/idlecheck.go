package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/idle"
)

func runIdleCheck(args []string) int {
	fs := flag.NewFlagSet("idle-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON estruturada")
	timeoutSec := fs.Int("timeout", 5, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de idle-check:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	result := idle.Check(ctx, idle.DefaultOptions())
	if *jsonOut {
		if err := result.RenderJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		result.Render(os.Stdout)
	}
	return result.ExitCode
}
