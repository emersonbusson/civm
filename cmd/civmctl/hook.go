package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/hook"
	"github.com/emersonbusson/civm/internal/hostdisk"
)

func runHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "uso: civmctl hook <install|job-started|job-completed> [flags]")
		return exitUsage
	}
	if args[0] == "install" {
		return runHookInstall(args[1:])
	}
	return runHookEvent(args)
}

func runHookEvent(args []string) int {
	event := hook.Event(args[0])
	fs := flag.NewFlagSet("hook "+string(event), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	execute := fs.Bool("execute", false, "aplicar limpeza; wrappers ACTIONS_RUNNER_HOOK_* usam true")
	jsonOut := fs.Bool("json", false, "saida JSON")
	preCleanupPct := fs.Int("pre-cleanup-pct", civm.DefaultPreCleanupPct, "job-started: limpar se disco usado >= pct")
	hardFailPct := fs.Int("hard-fail-pct", civm.DefaultHardFailPct, "job-started: rejeitar job se disco usado >= pct apos limpeza")
	minFreeGB := fs.Int("min-free-gb", civm.DefaultMinFreeGB, "job-started: full-clean se GB livre < N (piso clean-slate; 0 desliga)")
	hostMetricsPath := fs.String("host-metrics-path", civm.DefaultHostMetricsPath, "snapshot de host-metrics lido pelo gate host-aware")
	timeoutMin := fs.Int("timeout", civm.DefaultCleanupTimeoutMinutes, "timeout em minutos")
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de hook:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	opts := hook.DefaultOptionsFromEnv(event)
	opts.Execute = *execute
	opts.PreCleanupPct = *preCleanupPct
	opts.HardFailPct = *hardFailPct
	opts.MinFreeGB = *minFreeGB
	opts.HostDiskFn = func() (hostdisk.Report, error) {
		o := hostdisk.DefaultOptions()
		o.Path = *hostMetricsPath
		return hostdisk.Check(o)
	}
	res := hook.Run(ctx, opts)
	if *jsonOut {
		if err := hook.RenderJSON(os.Stdout, res); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		hook.RenderText(os.Stdout, res)
	}
	return res.ExitCode
}

func runHookInstall(args []string) int {
	fs := flag.NewFlagSet("hook install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	defaults := hook.DefaultInstallOptions()
	execute := fs.Bool("execute", false, "instalar wrappers e atualizar .env dos runners")
	jsonOut := fs.Bool("json", false, "saida JSON")
	hooksDir := fs.String("hooks-dir", defaults.HooksDir, "diretorio dos hooks ACTIONS_RUNNER_HOOK_*")
	civmctlPath := fs.String("civmctl-path", defaults.CivmctlPath, "binario invocado pelos scripts de hook")
	runnerGlob := fs.String("runner-glob", defaults.RunnerGlob, "glob dos diretorios actions-runner*")
	deploySource := fs.String("deploy-source", defaults.DeploySourceDir, "diretorio com deploy/bin/civm-safedelete e deploy/sudoers.d/civm-cleanup")
	noRestart := fs.Bool("no-restart", false, "nao reiniciar services actions.runner.*")
	timeoutMin := fs.Int("timeout", civm.DefaultRunnerTimeoutMinutes, "timeout em minutos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de hook install:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	opts := defaults
	opts.Execute = *execute
	opts.HooksDir = *hooksDir
	opts.CivmctlPath = *civmctlPath
	opts.RunnerGlob = *runnerGlob
	opts.DeploySourceDir = *deploySource
	opts.RestartRunners = !*noRestart
	res := hook.Install(ctx, opts)
	if *jsonOut {
		if err := hook.RenderInstallJSON(os.Stdout, res); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		hook.RenderInstallText(os.Stdout, res)
	}
	if res.Error != "" {
		return 1
	}
	return 0
}
