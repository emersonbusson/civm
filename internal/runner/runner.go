// Package runner orchestrates the full GitHub Actions self-hosted runner
// install flow: download tarball, extract, ./config.sh, svc.sh install,
// svc.sh start. Designed for "1 runner per peer-repo on the same VM".
package runner

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/idle"
)

// AddOptions controls a single runner installation.
type AddOptions struct {
	Repo          string // "other/peer"
	Token         string // registration token (efêmero ~1h)
	Short         string // suffix do diretorio: ~/actions-runner-<short>
	Label         string // CSV de labels (default: "civm")
	RunnerVersion string // ex: "2.336.0"
	RunnerSHA256  string // sha256 do actions-runner-linux-x64 tarball
	BaseDir       string // ex: "/home/emdev"
	RunAsUser     string // ex: "emdev" (passa para svc.sh install)
	Execute       bool   // false = dry-run; true = aplica
	RunFn         func(ctx context.Context, name string, args ...string) ([]byte, error)
	SHA256FileFn  func(path string) (string, error)
}

// DefaultOptions returns sane production defaults.
//
// Label "civm" alinhado com nome do repo infra (auto-explicativo:
// `runs-on: [self-hosted, civm]` em qualquer peer aponta pra
// runner mantido pelo civm). Migracao 2026-05-10 substituiu label
// legacy "legacy-ci" por "civm" em todos peers + runners.
func DefaultOptions() AddOptions {
	return AddOptions{
		Label:         "civm",
		RunnerVersion: civm.DefaultRunnerVersion,
		RunnerSHA256:  civm.DefaultRunnerLinuxX64SHA256,
		Execute:       false,
		RunFn:         defaultRun,
		SHA256FileFn:  civm.FileSHA256,
	}
}

// Step is one action in the install flow.
type Step struct {
	Name        string
	Description string
	WouldDo     string // resumo da acao em dry-run
	Apply       func(ctx context.Context) error
}

// Result captures step outcome.
type Result struct {
	Name        string
	Description string
	Executed    bool
	WouldDo     string
	Err         error
}

// Add runs the full install flow (or simulates it when Execute=false).
func Add(ctx context.Context, opts AddOptions) ([]Result, error) {
	if opts.RunnerVersion != civm.DefaultRunnerVersion && opts.RunnerSHA256 == civm.DefaultRunnerLinuxX64SHA256 {
		opts.RunnerSHA256 = ""
	}
	if opts.RunnerSHA256 == "" {
		if expected, ok := civm.RunnerLinuxX64SHA256(opts.RunnerVersion); ok {
			opts.RunnerSHA256 = expected
		}
	}
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.SHA256FileFn == nil {
		opts.SHA256FileFn = civm.FileSHA256
	}
	dir := filepath.Join(opts.BaseDir, "actions-runner-"+opts.Short)
	tarball := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-linux-x64-%s.tar.gz",
		opts.RunnerVersion, opts.RunnerVersion)
	url := fmt.Sprintf("https://github.com/%s", opts.Repo)
	// Naming padrao: civm-<short>. Ex: civm-peer, civm-app.
	// Para o proprio repo civm: convencao --short=self -> civm-self.
	name := "civm-" + opts.Short
	steps := []Step{
		{
			Name:        "mkdir_dir",
			Description: "Cria diretorio dedicado " + dir,
			WouldDo:     "mkdir -p " + dir,
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "mkdir", "-p", dir)
				return err
			},
		},
		{
			Name:        "download_runner",
			Description: "Baixa actions/runner v" + opts.RunnerVersion,
			WouldDo:     "curl -fsSL -o " + dir + "/runner.tar.gz " + tarball,
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "curl", "-fsSL", "-o", dir+"/runner.tar.gz", tarball)
				return err
			},
		},
		{
			Name:        "verify_runner_sha256",
			Description: "Verifica SHA256 do actions/runner v" + opts.RunnerVersion,
			WouldDo:     "sha256sum " + dir + "/runner.tar.gz",
			Apply: func(ctx context.Context) error {
				actual, err := opts.SHA256FileFn(dir + "/runner.tar.gz")
				if err != nil {
					return fmt.Errorf("sha256 %s/runner.tar.gz: %w", dir, err)
				}
				return civm.VerifySHA256(actual, opts.RunnerSHA256, "actions/runner v"+opts.RunnerVersion)
			},
		},
		{
			Name:        "extract_runner",
			Description: "Extrai tarball em " + dir,
			WouldDo:     "tar -C " + dir + " -xzf " + dir + "/runner.tar.gz && rm " + dir + "/runner.tar.gz",
			Apply: func(ctx context.Context) error {
				if _, err := opts.RunFn(ctx, "tar", "-C", dir, "-xzf", dir+"/runner.tar.gz"); err != nil {
					return err
				}
				_, err := opts.RunFn(ctx, "rm", dir+"/runner.tar.gz")
				return err
			},
		},
		{
			Name:        "config_runner",
			Description: "config.sh --unattended --labels " + opts.Label + " --name " + name,
			WouldDo:     fmt.Sprintf("(cd %s && ./config.sh --unattended --url %s --token *** --labels %s --name %s --work _work --replace)", dir, url, opts.Label, name),
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "sh", "-c", fmt.Sprintf(
					"cd %q && ./config.sh --unattended --url %q --token %q --labels %q --name %q --work _work --replace",
					dir, url, opts.Token, opts.Label, name))
				return err
			},
		},
		{
			Name:        "install_service",
			Description: "sudo svc.sh install " + opts.RunAsUser,
			WouldDo:     fmt.Sprintf("(cd %s && sudo ./svc.sh install %s)", dir, opts.RunAsUser),
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "sudo", "sh", "-c", fmt.Sprintf("cd %q && ./svc.sh install %q", dir, opts.RunAsUser))
				return err
			},
		},
		{
			Name:        "start_service",
			Description: "sudo svc.sh start",
			WouldDo:     fmt.Sprintf("(cd %s && sudo ./svc.sh start)", dir),
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "sudo", "sh", "-c", fmt.Sprintf("cd %q && ./svc.sh start", dir))
				return err
			},
		},
	}
	results := make([]Result, 0, len(steps))
	for _, s := range steps {
		r := Result{Name: s.Name, Description: s.Description, WouldDo: s.WouldDo}
		if !opts.Execute {
			results = append(results, r)
			continue
		}
		if err := s.Apply(ctx); err != nil {
			r.Err = err
			results = append(results, r)
			break
		}
		r.Executed = true
		results = append(results, r)
	}
	return results, nil
}

func validateOptions(opts AddOptions) error {
	if err := civm.ValidateRepo(opts.Repo); err != nil {
		return err
	}
	if opts.Token == "" {
		return fmt.Errorf("--token obrigatorio")
	}
	if err := civm.ValidateShort(opts.Short); err != nil {
		return err
	}
	if err := civm.ValidateLabels(opts.Label); err != nil {
		return err
	}
	if err := civm.ValidateSemver(opts.RunnerVersion, "--runner-version"); err != nil {
		return err
	}
	if strings.TrimSpace(opts.RunnerSHA256) == "" {
		return fmt.Errorf("--runner-sha256 obrigatorio para actions/runner v%s", opts.RunnerVersion)
	}
	cleanBase, err := civm.CleanDir(opts.BaseDir, "--base-dir")
	if err != nil {
		return err
	}
	if cleanBase != opts.BaseDir {
		return fmt.Errorf("--base-dir deve estar normalizado, got %q want %q", opts.BaseDir, cleanBase)
	}
	if err := civm.ValidateUserName(opts.RunAsUser); err != nil {
		return err
	}
	return nil
}

// RenderTable writes results human-readable.
func RenderTable(results []Result, opts AddOptions, w io.Writer) {
	mode := "DRY-RUN"
	if opts.Execute {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "Modo: %s\n", mode)
	fmt.Fprintf(w, "Repo: %s | Short: %s | Label: %s | Runner: v%s\n\n",
		opts.Repo, opts.Short, opts.Label, opts.RunnerVersion)
	for _, r := range results {
		status := "(seria-aplicado)"
		switch {
		case r.Err != nil:
			status = "erro: " + r.Err.Error()
		case r.Executed:
			status = "aplicado"
		}
		fmt.Fprintf(w, "  %-18s %s\n", r.Name, status)
		if !opts.Execute {
			fmt.Fprintf(w, "    -> %s\n", r.WouldDo)
		}
	}
	fmt.Fprintln(w)
	if !opts.Execute {
		fmt.Fprintln(w, "Para aplicar: rode novamente com --execute")
	}
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ==== Remove ====

// RemoveOptions controls a runner removal.
type RemoveOptions struct {
	Short          string // suffix do diretorio: ~/actions-runner-<short>
	Token          string // remove-token (efemero, gh api .../runners/remove-token)
	BaseDir        string // ex: "/home/emdev"
	Execute        bool   // false = dry-run
	IdleProbeDelay time.Duration
	RunFn          func(ctx context.Context, name string, args ...string) ([]byte, error)
	ActivityFn     func(ctx context.Context) ([]idle.Activity, error)
}

// DefaultRemoveOptions returns sane defaults.
func DefaultRemoveOptions() RemoveOptions {
	return RemoveOptions{
		Execute:        false,
		IdleProbeDelay: 2 * time.Second,
		RunFn:          defaultRun,
		ActivityFn:     idle.DefaultActivities,
	}
}

// Remove undoes Add. Service stop/uninstall fail closed so civmctl does not
// remove a runner directory while systemd may still have an active unit.
func Remove(ctx context.Context, opts RemoveOptions) ([]Result, error) {
	if err := validateRemoveOptions(opts); err != nil {
		return nil, err
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.ActivityFn == nil {
		opts.ActivityFn = idle.DefaultActivities
	}
	dir := filepath.Join(opts.BaseDir, "actions-runner-"+opts.Short)
	if opts.Execute {
		if err := ensureMutationIdle(ctx, opts.ActivityFn, opts.IdleProbeDelay); err != nil {
			return []Result{{
				Name:        "host_idle",
				Description: "Confirma host sem job/build ativo",
				Err:         err,
			}}, nil
		}
	}
	steps := []Step{
		{
			Name:        "stop_service",
			Description: "sudo svc.sh stop",
			WouldDo:     fmt.Sprintf("(cd %s && sudo ./svc.sh stop)", dir),
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "sudo", filepath.Join(dir, "svc.sh"), "stop")
				return err
			},
		},
		{
			Name:        "uninstall_service",
			Description: "sudo svc.sh uninstall",
			WouldDo:     fmt.Sprintf("(cd %s && sudo ./svc.sh uninstall)", dir),
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "sudo", "sh", "-c", fmt.Sprintf("cd %q && ./svc.sh uninstall", dir))
				return err
			},
		},
		{
			Name:        "config_remove",
			Description: "config.sh remove --token=*** (deregister no GitHub)",
			WouldDo:     fmt.Sprintf("%s remove --token=***", filepath.Join(dir, "config.sh")),
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "sh", "-c", fmt.Sprintf("cd %q && ./config.sh remove --token %q", dir, opts.Token))
				return err
			},
		},
		{
			Name:        "remove_dir",
			Description: "rm -rf " + dir,
			WouldDo:     "rm -rf " + dir,
			Apply: func(ctx context.Context) error {
				_, err := opts.RunFn(ctx, "rm", "-rf", dir)
				return err
			},
		},
	}
	results := make([]Result, 0, len(steps))
	for _, s := range steps {
		r := Result{Name: s.Name, Description: s.Description, WouldDo: s.WouldDo}
		if !opts.Execute {
			results = append(results, r)
			continue
		}
		if err := s.Apply(ctx); err != nil {
			r.Err = err
			results = append(results, r)
			break
		} else {
			r.Executed = true
		}
		results = append(results, r)
	}
	return results, nil
}

func validateRemoveOptions(opts RemoveOptions) error {
	if err := civm.ValidateShort(opts.Short); err != nil {
		return err
	}
	cleanBase, err := civm.CleanDir(opts.BaseDir, "--base-dir")
	if err != nil {
		return err
	}
	if cleanBase != opts.BaseDir {
		return fmt.Errorf("--base-dir deve estar normalizado, got %q want %q", opts.BaseDir, cleanBase)
	}
	if opts.Token == "" {
		return fmt.Errorf("--token obrigatorio (gh api -X POST .../actions/runners/remove-token)")
	}
	return nil
}

// RenderRemoveTable writes remove results human-readable.
func RenderRemoveTable(results []Result, opts RemoveOptions, w io.Writer) {
	mode := "DRY-RUN"
	if opts.Execute {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "Modo: %s\n", mode)
	fmt.Fprintf(w, "Short: %s | Dir: %s/actions-runner-%s\n\n",
		opts.Short, opts.BaseDir, opts.Short)
	for _, r := range results {
		status := "(seria-aplicado)"
		switch {
		case r.Err != nil:
			status = "erro: " + r.Err.Error()
		case r.Executed:
			status = "aplicado"
		}
		fmt.Fprintf(w, "  %-20s %s\n", r.Name, status)
		if !opts.Execute {
			fmt.Fprintf(w, "    -> %s\n", r.WouldDo)
		}
	}
	fmt.Fprintln(w)
	if !opts.Execute {
		fmt.Fprintln(w, "Para aplicar: rode novamente com --execute")
	}
}
