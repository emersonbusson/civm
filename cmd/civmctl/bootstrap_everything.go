package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/emersonbusson/civm/internal/bootstrap"
	"github.com/emersonbusson/civm/internal/civm"
)

// runBootstrapEverything wrappa bootstrap + cp dos systemd units.
// Pré-requisito: repo civm clonado em --units-source (ou /opt/civm
// como default). civmctl ja deve estar em /usr/local/bin (use
// 'go build -o' ou install standalone antes).
//
// Diferença vs `civmctl bootstrap`: garante que os arquivos
// .service/.timer estao em /etc/systemd/system/ ANTES de tentar
// systemctl enable. Atual `bootstrap` assume que admin ja copiou.
func runBootstrapEverything(args []string) int {
	fs := flag.NewFlagSet("bootstrap-everything", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	unitsSource := fs.String("units-source", civm.DefaultUnitsSourceDir,
		"diretorio com .service/.timer files (ex: /opt/civm/deploy/systemd)")
	execute := fs.Bool("execute", false, "aplicar (default: dry-run)")
	watchdog := fs.Bool("watchdog", true, "habilitar disk-watchdog timer")
	runnerWatchdog := fs.Bool("runner-watchdog", true, "habilitar runner-watchdog timer")
	reverseWatchdog := fs.Bool("reverse-watchdog", true, "habilitar reverse-watchdog timer")
	metricsTimer := fs.Bool("metrics-timer", true, "habilitar metrics timer")
	timeoutMin := fs.Int("timeout", civm.DefaultCleanupTimeoutMinutes, "timeout total em minutos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args:", err)
		return exitUsage
	}

	if *execute {
		fmt.Fprintln(os.Stderr, "AVISO: --execute vai modificar /etc/systemd/system/, apt install,")
		fmt.Fprintln(os.Stderr, "instalar Go em /usr/local/go, habilitar timers. Ctrl+C em 5s...")
		time.Sleep(5 * time.Second)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()

	steps := buildBootstrapEverythingSteps(*unitsSource, *watchdog, *runnerWatchdog, *reverseWatchdog, *metricsTimer, *execute)
	results := runEverythingSteps(ctx, steps, *execute)
	renderEverythingTable(results, *execute, os.Stdout)

	for _, r := range results {
		if r.Err != nil {
			return 1
		}
	}
	return 0
}

type everythingStep struct {
	Name    string
	WouldDo string
	Apply   func(ctx context.Context) error
}

type everythingResult struct {
	Name     string
	WouldDo  string
	Executed bool
	Err      error
}

func buildBootstrapEverythingSteps(unitsSource string, watchdog, runnerWatchdog, reverseWatchdog, metricsTimer, execute bool) []everythingStep {
	systemdDest := civm.DefaultSystemdDir
	cleanUnitsSource, unitsSourceErr := civm.CleanDir(unitsSource, "--units-source")
	timerNames := []string{"civmctl-cleanup"}
	if watchdog {
		timerNames = append(timerNames, "civmctl-disk-watchdog")
	}
	if runnerWatchdog {
		timerNames = append(timerNames, "civmctl-runner-watchdog")
	}
	if reverseWatchdog {
		timerNames = append(timerNames, "civmctl-reverse-watchdog")
	}
	if metricsTimer {
		timerNames = append(timerNames, "civmctl-metrics")
	}
	timerNames = append(timerNames, "civmctl-run-reaper")

	steps := []everythingStep{
		{
			Name:    "verify_civmctl",
			WouldDo: "test -x " + civm.DefaultCivmctlPath + " (systemd usa esse path)",
			Apply: func(ctx context.Context) error {
				info, err := os.Stat(civm.DefaultCivmctlPath)
				if err != nil {
					return fmt.Errorf("civmctl nao encontrado em %s: %w", civm.DefaultCivmctlPath, err)
				}
				if info.IsDir() || info.Mode()&0111 == 0 {
					return fmt.Errorf("civmctl em %s nao e arquivo executavel", civm.DefaultCivmctlPath)
				}
				return nil
			},
		},
		{
			Name:    "verify_units_source",
			WouldDo: "ls " + cleanUnitsSource + "/civmctl-*.{service,timer}",
			Apply: func(ctx context.Context) error {
				if unitsSourceErr != nil {
					return unitsSourceErr
				}
				if _, err := os.Stat(cleanUnitsSource); err != nil {
					return fmt.Errorf("units-source %s nao existe: %w (use --units-source pra customizar)", cleanUnitsSource, err)
				}
				return nil
			},
		},
	}

	for _, name := range timerNames {
		name := name
		steps = append(steps,
			everythingStep{
				Name:    "cp_" + name + "_service",
				WouldDo: fmt.Sprintf("sudo cp %s/%s.service %s/", cleanUnitsSource, name, systemdDest),
				Apply: func(ctx context.Context) error {
					src := filepath.Join(cleanUnitsSource, name+".service")
					dst := filepath.Join(systemdDest, name+".service")
					return runCommand(ctx, "sudo", "cp", src, dst)
				},
			},
			everythingStep{
				Name:    "cp_" + name + "_timer",
				WouldDo: fmt.Sprintf("sudo cp %s/%s.timer %s/", cleanUnitsSource, name, systemdDest),
				Apply: func(ctx context.Context) error {
					src := filepath.Join(cleanUnitsSource, name+".timer")
					dst := filepath.Join(systemdDest, name+".timer")
					return runCommand(ctx, "sudo", "cp", src, dst)
				},
			},
		)
	}

	steps = append(steps,
		everythingStep{
			Name:    "daemon_reload",
			WouldDo: "sudo systemctl daemon-reload",
			Apply: func(ctx context.Context) error {
				return runCommand(ctx, "sudo", "systemctl", "daemon-reload")
			},
		},
		everythingStep{
			Name:    "bootstrap_run",
			WouldDo: "civmctl bootstrap " + execFlag(execute) + " --watchdog=" + boolStr(watchdog) + " --runner-watchdog=" + boolStr(runnerWatchdog) + " --reverse-watchdog=" + boolStr(reverseWatchdog) + " --metrics-timer=" + boolStr(metricsTimer),
			Apply: func(ctx context.Context) error {
				opts := bootstrap.DefaultOptions()
				opts.Execute = true
				opts.WatchdogTimer = watchdog
				opts.RunnerWatchdog = runnerWatchdog
				opts.ReverseWatchdog = reverseWatchdog
				opts.MetricsTimer = metricsTimer
				results := bootstrap.Run(ctx, opts)
				for _, r := range results {
					if r.Err != nil {
						return fmt.Errorf("bootstrap step %s: %w", r.Name, r.Err)
					}
				}
				return nil
			},
		},
	)

	return steps
}

func runEverythingSteps(ctx context.Context, steps []everythingStep, execute bool) []everythingResult {
	out := make([]everythingResult, 0, len(steps))
	for _, s := range steps {
		r := everythingResult{Name: s.Name, WouldDo: s.WouldDo}
		if !execute {
			out = append(out, r)
			continue
		}
		if err := s.Apply(ctx); err != nil {
			r.Err = err
			out = append(out, r)
			break
		}
		r.Executed = true
		out = append(out, r)
	}
	return out
}

func renderEverythingTable(results []everythingResult, execute bool, w io.Writer) {
	mode := "DRY-RUN"
	if execute {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "Modo: %s (bootstrap-everything)\n\n", mode)
	for _, r := range results {
		status := "(seria-aplicado)"
		switch {
		case r.Err != nil:
			status = "erro: " + r.Err.Error()
		case r.Executed:
			status = "aplicado"
		}
		fmt.Fprintf(w, "  %-26s %s\n", r.Name, status)
		if !execute {
			fmt.Fprintf(w, "    -> %s\n", r.WouldDo)
		}
	}
	fmt.Fprintln(w)
	if !execute {
		fmt.Fprintln(w, "Para aplicar: sudo civmctl bootstrap-everything --execute")
	}
}

func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (output: %s)", name, joinArgs(args), err, string(out))
	}
	return nil
}

func joinArgs(args []string) string {
	out := ""
	for i, arg := range args {
		if i > 0 {
			out += " "
		}
		out += arg
	}
	return out
}

func execFlag(execute bool) string {
	if execute {
		return "--execute"
	}
	return ""
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
