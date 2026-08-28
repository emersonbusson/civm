package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/selfupgrade"
)

func runSelfUpgrade(args []string) int {
	fs := flag.NewFlagSet("self-upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	source := fs.String("source", "/opt/civm", "diretório do checkout do source")
	target := fs.String("target", "/usr/local/bin/civmctl", "caminho final do binário")
	execute := fs.Bool("execute", false, "aplicar a substituição (default: dry-run)")
	jsonOut := fs.Bool("json", false, "saída JSON")
	timeoutMin := fs.Int("timeout", 5, "timeout do build em minutos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de self-upgrade:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	opts := selfupgrade.DefaultOptions()
	opts.SourceDir = *source
	opts.Target = *target
	opts.Execute = *execute
	opts.Timeout = time.Duration(*timeoutMin) * time.Minute
	res := selfupgrade.Run(ctx, opts)
	if *jsonOut {
		if err := selfupgrade.RenderJSON(os.Stdout, res); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		selfupgrade.RenderText(os.Stdout, res)
	}
	if res.Error != "" {
		return 1
	}
	return 0
}
