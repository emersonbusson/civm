package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/doctor"
)

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	defaults := defaultDoctorCLIOptions()
	reposRaw := fs.String("repos", "auto", "repos: auto, default, none ou owner/repo separados por virgula")
	workflow := fs.String("workflow", "ci.yml", "nome do workflow file")
	hooksDir := fs.String("hooks-dir", defaults.HooksDir, "diretorio dos hooks ACTIONS_RUNNER_HOOK_*")
	civmctlPath := fs.String("civmctl-path", defaults.CivmctlPath, "binario esperado nos scripts de hook")
	runnerGlob := fs.String("runner-glob", defaults.RunnerGlob, "glob dos diretorios actions-runner*")
	jsonOut := fs.Bool("json", false, "saida JSON estruturada")
	timeoutSec := fs.Int("timeout", 20, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de doctor:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	opts := defaults
	if err := configureDoctorRepos(*reposRaw, &opts); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de doctor:", err)
		return exitUsage
	}
	opts.WorkflowFile = *workflow
	opts.HooksDir = *hooksDir
	opts.CivmctlPath = *civmctlPath
	opts.RunnerGlob = *runnerGlob
	report, err := doctor.Collect(ctx, opts)
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

func defaultDoctorCLIOptions() doctor.Options {
	return doctor.DefaultOptions()
}

func configureDoctorRepos(raw string, opts *doctor.Options) error {
	mode := strings.TrimSpace(raw)
	switch mode {
	case "auto":
		opts.InferRepos = true
		opts.Repos = nil
	case "", "none":
		opts.InferRepos = false
		opts.Repos = nil
	case "default":
		opts.InferRepos = false
		opts.Repos = append([]string(nil), doctor.DefaultRepos...)
	default:
		opts.InferRepos = false
		opts.Repos = splitCSV(raw)
	}
	return nil
}

func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
