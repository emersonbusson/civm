package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/hook"
	"github.com/emersonbusson/civm/internal/runner"
)

func runRunner(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "uso: civmctl runner <add|list|remove|restart|upgrade|watchdog> [flags]")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "add":
		return runRunnerAdd(rest)
	case "list":
		return runRunnerList(rest)
	case "remove":
		return runRunnerRemove(rest)
	case "restart":
		return runRunnerRestart(rest)
	case "upgrade":
		return runRunnerUpgrade(rest)
	case "watchdog":
		return runRunnerWatchdog(rest)
	default:
		fmt.Fprintf(os.Stderr, "subcomando desconhecido: %s\n", sub)
		return exitUsage
	}
}

func runRunnerWatchdog(args []string) int {
	fs := flag.NewFlagSet("runner watchdog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	execute := fs.Bool("execute", false, "aplicar reparos e reruns seguros (default: dry-run)")
	reposRaw := fs.String("repos", "auto", "repos permitidos: auto ou owner/repo separados por virgula")
	rerunNetworkFailures := fs.Bool("rerun-network-failures", false, "rerodar uma vez falhas transientes de rede/checkout")
	maxRunAge := fs.Duration("max-run-age", runner.DefaultWatchdogMaxRunAge, "idade maxima de run elegivel para rerun")
	networkTimeout := fs.Duration("network-timeout", 10*time.Second, "timeout do probe de conectividade GitHub")
	restartDelay := fs.Duration("restart-delay", 10*time.Second, "delay entre restart systemd e is-active")
	queueStallDwell := fs.Duration(
		"queue-stall-dwell",
		runner.DefaultQueueStallDwell,
		"janela minima da mesma fila elegivel antes de reiniciar sessao online/ociosa",
	)
	markerPath := fs.String("marker-path", runner.DefaultWatchdogMarkerPath, "arquivo local de marker anti-loop")
	hooksDir := fs.String("hooks-dir", hook.DefaultHooksDir, "diretorio dos hooks gerenciados")
	runnerGlob := fs.String("runner-glob", hook.DefaultRunnerGlob, "glob dos diretorios actions-runner*")
	civmctlPath := fs.String("civmctl-path", hook.DefaultCivmctlBin, "binario usado pelos scripts de hook")
	jsonOut := fs.Bool("json", false, "saida JSON estruturada")
	timeout := fs.Duration("timeout", 90*time.Second, "timeout total do watchdog")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de runner watchdog:", err)
		return exitUsage
	}
	if *maxRunAge <= 0 {
		fmt.Fprintln(os.Stderr, "erro nos args de runner watchdog: --max-run-age deve ser >0")
		return exitUsage
	}
	if *queueStallDwell <= 0 {
		fmt.Fprintln(os.Stderr, "erro nos args de runner watchdog: --queue-stall-dwell deve ser >0")
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	opts := runner.DefaultWatchdogOptions()
	opts.Execute = *execute
	opts.RerunNetworkFailures = *rerunNetworkFailures
	opts.MaxRunAge = *maxRunAge
	opts.NetworkTimeout = *networkTimeout
	opts.RestartDelay = *restartDelay
	opts.QueueStallDwell = *queueStallDwell
	opts.MarkerPath = *markerPath
	opts.HooksDir = *hooksDir
	opts.RunnerGlob = *runnerGlob
	opts.CivmctlPath = *civmctlPath
	if err := configureRunnerWatchdogRepos(*reposRaw, &opts); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de runner watchdog:", err)
		return exitUsage
	}
	report := runner.Watchdog(ctx, opts)
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

func runRunnerUpgrade(args []string) int {
	fs := flag.NewFlagSet("runner upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	short := fs.String("short", "", "suffix curto (ex: api, web, worker)")
	unit := fs.String("unit", "", "unit name explícito (sobreescreve --short)")
	dir := fs.String("dir", "", "diretorio do runner explicito (override do guess BaseDir/actions-runner-Short)")
	newVersion := fs.String("new-version", "", "nova versao (ex: 2.335.0)")
	runnerSHA256 := fs.String("runner-sha256", "", "sha256 do tarball actions-runner-linux-x64 (default: pin conhecido)")
	baseDir := fs.String("base-dir", "", "base dir (default: $HOME)")
	verifySec := fs.Int("verify-delay", civm.DefaultUpgradeVerifySeconds, "segundos entre start e is-active check")
	execute := fs.Bool("execute", false, "aplicar (default: dry-run)")
	timeoutMin := fs.Int("timeout", civm.DefaultRunnerTimeoutMinutes, "timeout em minutos (download pode demorar)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de runner upgrade:", err)
		return exitUsage
	}
	if *baseDir == "" {
		*baseDir = userHomeOrDefault()
	}
	opts := runner.DefaultUpgradeOptions()
	opts.Short = *short
	opts.Unit = *unit
	opts.Dir = *dir
	opts.NewVersion = *newVersion
	opts.RunnerSHA256 = *runnerSHA256
	opts.BaseDir = *baseDir
	opts.VerifyDelay = time.Duration(*verifySec) * time.Second
	opts.Execute = *execute
	if *execute {
		fmt.Fprintln(os.Stderr, "AVISO: --execute vai parar runner, baixar tarball, sobrescrever binarios e reiniciar.")
		fmt.Fprintln(os.Stderr, "Se houver job/build ativo, civmctl aborta fail-closed antes da mutacao.")
		fmt.Fprintln(os.Stderr, "Pressione Ctrl+C em 5s pra cancelar...")
		time.Sleep(5 * time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	r, err := runner.Upgrade(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return exitUsage
	}
	runner.RenderUpgradeTable(r, opts, os.Stdout)
	if r.Err != nil {
		return 1
	}
	return 0
}

func runRunnerRestart(args []string) int {
	fs := flag.NewFlagSet("runner restart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	short := fs.String("short", "", "suffix curto (ex: api, web, worker)")
	unit := fs.String("unit", "", "unit name explícito (sobreescreve --short)")
	verifySec := fs.Int("verify-delay", civm.DefaultRestartVerifySeconds, "segundos entre restart e is-active check")
	execute := fs.Bool("execute", false, "aplicar (default: dry-run)")
	timeoutSec := fs.Int("timeout", civm.DefaultRestartTimeoutSeconds, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de runner restart:", err)
		return exitUsage
	}
	opts := runner.DefaultRestartOptions()
	opts.Short = *short
	opts.Unit = *unit
	opts.VerifyDelay = time.Duration(*verifySec) * time.Second
	opts.Execute = *execute
	if *execute {
		fmt.Fprintln(os.Stderr, "AVISO: --execute vai parar e reiniciar o runner systemd.")
		fmt.Fprintln(os.Stderr, "Se houver job/build ativo, civmctl aborta fail-closed antes da mutacao.")
		fmt.Fprintln(os.Stderr, "Pressione Ctrl+C em 3s...")
		time.Sleep(3 * time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	r, err := runner.Restart(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return exitUsage
	}
	runner.RenderRestartTable(r, opts, os.Stdout)
	if r.Err != nil {
		return 1
	}
	return 0
}

func runRunnerAdd(args []string) int {
	fs := flag.NewFlagSet("runner add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "owner/repo (ex: owner/repo)")
	token := fs.String("token", "", "registration token (efemero ~1h via gh api)")
	short := fs.String("short", "", "suffix curto do diretorio (ex: api, web)")
	label := fs.String("label", "civm", "labels CSV")
	runnerVersion := fs.String("runner-version", civm.DefaultRunnerVersion, "versao do actions/runner")
	runnerSHA256 := fs.String("runner-sha256", "", "sha256 do tarball actions-runner-linux-x64 (default: pin conhecido)")
	baseDir := fs.String("base-dir", "", "base dir (default: \\$HOME do user atual)")
	runAs := fs.String("run-as", "", "user que vai rodar o service (default: user atual)")
	execute := fs.Bool("execute", false, "aplicar (default: dry-run)")
	timeoutMin := fs.Int("timeout", civm.DefaultRunnerTimeoutMinutes, "timeout total em minutos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de runner add:", err)
		return exitUsage
	}
	if *baseDir == "" {
		*baseDir = userHomeOrDefault()
	}
	if *runAs == "" {
		*runAs = userNameOrDefault()
	}
	opts := runner.DefaultOptions()
	opts.Repo = *repo
	opts.Token = *token
	opts.Short = *short
	opts.Label = *label
	opts.RunnerVersion = *runnerVersion
	opts.RunnerSHA256 = *runnerSHA256
	opts.BaseDir = *baseDir
	opts.RunAsUser = *runAs
	opts.Execute = *execute
	if *execute {
		fmt.Fprintln(os.Stderr, "AVISO: --execute vai modificar o sistema (mkdir, curl, tar, sudo svc.sh).")
		fmt.Fprintln(os.Stderr, "Pressione Ctrl+C em 3s para abortar...")
		time.Sleep(3 * time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	results, err := runner.Add(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return exitUsage
	}
	runner.RenderTable(results, opts, os.Stdout)
	for _, r := range results {
		if r.Err != nil {
			return 1
		}
	}
	return 0
}

func runRunnerList(args []string) int {
	fs := flag.NewFlagSet("runner list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON estruturada")
	timeoutSec := fs.Int("timeout", 5, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de runner list:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	items, err := runner.List(ctx, runner.DefaultListOptions())
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return 1
	}
	if *jsonOut {
		if err := runner.RenderListJSON(items, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 2
		}
	} else {
		runner.RenderListTable(items, os.Stdout)
	}
	if len(items) == 0 {
		return 1
	}
	return 0
}

func runRunnerRemove(args []string) int {
	fs := flag.NewFlagSet("runner remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	short := fs.String("short", "", "suffix curto (ex: api, web, worker)")
	token := fs.String("token", "", "remove-token (gh api -X POST /repos/.../actions/runners/remove-token)")
	baseDir := fs.String("base-dir", "", "base dir (default: $HOME)")
	execute := fs.Bool("execute", false, "aplicar (default: dry-run)")
	timeoutMin := fs.Int("timeout", civm.DefaultRunnerRemoveTimeoutMinutes, "timeout em minutos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de runner remove:", err)
		return exitUsage
	}
	if *baseDir == "" {
		*baseDir = userHomeOrDefault()
	}
	opts := runner.DefaultRemoveOptions()
	opts.Short = *short
	opts.Token = *token
	opts.BaseDir = *baseDir
	opts.Execute = *execute
	if *execute {
		fmt.Fprintln(os.Stderr, "AVISO: --execute vai parar service + remover diretorio.")
		fmt.Fprintln(os.Stderr, "Se houver job/build ativo, civmctl aborta fail-closed antes da mutacao.")
		fmt.Fprintln(os.Stderr, "Pressione Ctrl+C em 3s para abortar...")
		time.Sleep(3 * time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()
	results, err := runner.Remove(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		return exitUsage
	}
	runner.RenderRemoveTable(results, opts, os.Stdout)
	for _, r := range results {
		if r.Err != nil {
			return 1
		}
	}
	return 0
}

func configureRunnerWatchdogRepos(raw string, opts *runner.WatchdogOptions) error {
	mode := strings.TrimSpace(raw)
	switch mode {
	case "auto":
		opts.InferRepos = true
		opts.Repos = nil
	case "":
		return fmt.Errorf("--repos deve ser auto ou owner/repo")
	default:
		opts.InferRepos = false
		opts.Repos = splitCSV(raw)
		if len(opts.Repos) == 0 {
			return fmt.Errorf("--repos deve informar pelo menos um repo")
		}
	}
	return nil
}

func userHomeOrDefault() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/home/runner"
}

func userNameOrDefault() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if h := os.Getenv("USER"); h != "" {
		return h
	}
	return "runner"
}
