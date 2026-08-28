package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/maintenance"
)

func runMaintenance(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "uso: civmctl maintenance <enter|exit> [--execute] [--strict] [--json] [--repos=a,b]")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "enter":
		return runMaintenanceAction(maintenance.Enter, "enter", rest)
	case "exit":
		return runMaintenanceAction(maintenance.Exit, "exit", rest)
	default:
		fmt.Fprintf(os.Stderr, "subcomando desconhecido: %s (use enter|exit)\n", sub)
		return exitUsage
	}
}

func runMaintenanceAction(
	action func(ctx context.Context, opts maintenance.Options) (maintenance.State, error),
	name string,
	args []string,
) int {
	fs := flag.NewFlagSet("maintenance "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	execute := fs.Bool("execute", false, "aplicar drain/restore (default: dry-run)")
	force := fs.Bool("force", false, "enter: drenar mesmo com host nao-ocioso")
	strict := fs.Bool("strict", false, "enter: exigir que todos os runners parem")
	jsonOut := fs.Bool("json", false, "saida JSON estruturada")
	reposRaw := fs.String("repos", "", "repos a drenar: vazio infere das units, ou owner/repo separados por virgula")
	statePath := fs.String("state-path", civm.DefaultMaintenanceStatePath, "arquivo de snapshot do drain")
	lockPath := fs.String("lock-path", civm.DefaultMaintenanceLockPath, "arquivo de flock anti-concorrencia")
	timeoutSec := fs.Int("timeout", civm.DefaultRestartTimeoutSeconds, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "erro nos args de maintenance %s: %v\n", name, err)
		return exitUsage
	}
	if *timeoutSec <= 0 {
		fmt.Fprintf(os.Stderr, "erro nos args de maintenance %s: --timeout deve ser >0\n", name)
		return exitUsage
	}
	if *strict && name != "enter" {
		fmt.Fprintln(os.Stderr, "erro nos args de maintenance exit: --strict so vale para enter")
		return exitUsage
	}
	if *strict && *force {
		fmt.Fprintln(os.Stderr, "erro nos args de maintenance enter: --strict e --force sao incompativeis")
		return exitUsage
	}

	opts := maintenance.DefaultOptions()
	opts.Execute = *execute
	opts.Force = *force
	opts.RequireStopped = *strict
	opts.StatePath = *statePath
	opts.LockPath = *lockPath
	opts.Repos = splitCSV(*reposRaw)

	if *execute {
		fmt.Fprintf(os.Stderr, "AVISO: --execute vai %s runners (systemctl + gh label).\n", name)
		fmt.Fprintln(os.Stderr, "Pressione Ctrl+C em 3s para abortar...")
		time.Sleep(3 * time.Second)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	state, err := action(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return 1
	}
	if *jsonOut {
		if err := maintenance.RenderJSON(os.Stdout, state); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		maintenance.RenderText(os.Stdout, name, state)
	}
	return 0
}
