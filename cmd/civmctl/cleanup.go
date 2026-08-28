package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/cleanup"
)

func runCleanup(args []string) int {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	execute := fs.Bool("execute", false, "aplicar mudancas (default: dry-run)")
	dryRun := fs.Bool("dry-run", false, "explicito dry-run (default ja e dry-run)")
	workDir := fs.String("work-dir", civm.DefaultWorkDir, "diretorio do runner")
	tmpDir := fs.String("tmp-dir", civm.DefaultTmpDir, "diretorio /tmp")
	codespaceDir := fs.String("codespace-dir", civm.DefaultCodespaceDir, "diretorio de clones manuais (sentinel: glob /home/*/codespace)")
	noDocker := fs.Bool("no-docker", false, "nao rodar docker prune")
	managedVolumes := fs.Bool(
		"managed-volumes",
		false,
		"boundary: remover volumes Compose gerenciados (exige admissao fechada)",
	)
	noApt := fs.Bool("no-apt", false, "nao rodar apt clean")
	timeoutMin := fs.Int("timeout", civm.DefaultCleanupTimeoutMinutes, "timeout em minutos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de cleanup:", err)
		return exitUsage
	}
	if *execute && *dryRun {
		fmt.Fprintln(os.Stderr, "erro: --execute e --dry-run sao mutuamente exclusivos")
		return exitUsage
	}
	if *managedVolumes && *noDocker {
		fmt.Fprintln(os.Stderr, "erro: --managed-volumes e --no-docker sao mutuamente exclusivos")
		return exitUsage
	}
	if *execute {
		fmt.Fprintln(os.Stderr, "AVISO: modo --execute vai DELETAR arquivos. Pressione Ctrl+C em 3s para abortar...")
		time.Sleep(3 * time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	opts := cleanup.DefaultOptions()
	opts.Execute = *execute
	opts.WorkDir = *workDir
	opts.TmpDir = *tmpDir
	opts.CodespaceDir = *codespaceDir
	opts.DockerPrune = !*noDocker
	opts.ManagedVolumePrune = *managedVolumes
	opts.AptClean = !*noApt
	actions := cleanup.Run(ctx, opts)
	cleanup.RenderTable(actions, opts, os.Stdout)
	for _, a := range actions {
		if a.Err != nil {
			return 1
		}
	}
	return 0
}
