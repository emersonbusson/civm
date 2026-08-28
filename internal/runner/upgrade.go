package runner

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/idle"
)

// UpgradeOptions controls a runner version upgrade.
type UpgradeOptions struct {
	Short          string // suffix curto
	Unit           string // unit name explícito (sobreescreve Short)
	Dir            string // diretorio do runner explicito (sobreescreve guess de Short)
	NewVersion     string // ex: "2.335.0"
	RunnerSHA256   string // sha256 do actions-runner-linux-x64 tarball
	BaseDir        string // ex: "/home/emdev"
	Execute        bool
	VerifyDelay    time.Duration
	IdleProbeDelay time.Duration
	RunFn          func(ctx context.Context, name string, args ...string) ([]byte, error)
	SHA256FileFn   func(path string) (string, error)
	ActivityFn     func(ctx context.Context) ([]idle.Activity, error)
	SleepFn        func(d time.Duration)
}

// DefaultUpgradeOptions returns sane defaults.
func DefaultUpgradeOptions() UpgradeOptions {
	return UpgradeOptions{
		VerifyDelay:    time.Duration(civm.DefaultUpgradeVerifySeconds) * time.Second,
		IdleProbeDelay: 2 * time.Second,
		Execute:        false,
		RunFn:          defaultRun,
		SHA256FileFn:   civm.FileSHA256,
		ActivityFn:     idle.DefaultActivities,
		SleepFn:        time.Sleep,
	}
}

// UpgradeResult captures upgrade outcome.
type UpgradeResult struct {
	Dir          string
	UnitResolved string
	OldVersion   string // detected before upgrade
	NewVersion   string
	StoppedOK    bool
	DownloadedOK bool
	ExtractedOK  bool
	StartedOK    bool
	ActiveAfter  bool
	WouldDo      string
	Err          error
}

// Upgrade does in-place version bump preserving runner identity (.runner,
// .credentials, _work). Stop service → download new tarball → tar over
// existing dir (excluding state files) → start service → verify.
//
// .runner, .credentials, .credentials_rsaparams, _work são PRESERVADOS
// (não vem no tarball, então tar -xzf não os toca). actions-runner-linux
// só inclui binários e scripts.
func Upgrade(ctx context.Context, opts UpgradeOptions) (UpgradeResult, error) {
	if opts.RunnerSHA256 == "" {
		if expected, ok := civm.RunnerLinuxX64SHA256(opts.NewVersion); ok {
			opts.RunnerSHA256 = expected
		}
	}
	if err := validateUpgradeOptions(opts); err != nil {
		return UpgradeResult{}, err
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.SHA256FileFn == nil {
		opts.SHA256FileFn = civm.FileSHA256
	}
	if opts.ActivityFn == nil {
		opts.ActivityFn = idle.DefaultActivities
	}
	if opts.SleepFn == nil {
		opts.SleepFn = time.Sleep
	}
	if opts.VerifyDelay == 0 {
		opts.VerifyDelay = time.Duration(civm.DefaultUpgradeVerifySeconds) * time.Second
	}
	r := UpgradeResult{NewVersion: opts.NewVersion}

	// 1. Resolve unit + dir
	if opts.Unit != "" {
		r.UnitResolved = opts.Unit
	} else {
		unit, err := resolveUnitByShort(ctx, RestartOptions{Short: opts.Short, RunFn: opts.RunFn})
		if err != nil {
			r.Err = err
			return r, nil
		}
		r.UnitResolved = unit
	}
	// Dir explícito sobreescreve guess. Necessário pra runners legacy
	// que nao seguem convenção ~/actions-runner-<short>/ (ex: civm-1
	// historicamente em ~/actions-runner/).
	if opts.Dir != "" {
		r.Dir = opts.Dir
	} else {
		r.Dir = guessDirFromShort(opts.BaseDir, opts.Short)
	}

	// 2. Detect old version (best-effort, ignore errors)
	if oldOut, err := opts.RunFn(ctx, "head", "-3", filepath.Join(r.Dir, "bin/Runner.Listener.runtimeconfig.json")); err == nil {
		r.OldVersion = strings.TrimSpace(string(oldOut))
		if len(r.OldVersion) > 80 {
			r.OldVersion = r.OldVersion[:80] + "..."
		}
	}

	tarball := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-linux-x64-%s.tar.gz",
		opts.NewVersion, opts.NewVersion)
	tarPath := r.Dir + "/runner-upgrade.tar.gz"

	r.WouldDo = fmt.Sprintf("(1) sudo systemctl stop %s; (2) curl -fsSL -o %s %s; (3) verify sha256; (4) tar -C %s -xzf %s (preserva .runner/.credentials/_work); (5) sudo systemctl start %s; (6) sleep %s && systemctl is-active %s",
		r.UnitResolved, tarPath, tarball, r.Dir, tarPath, r.UnitResolved, opts.VerifyDelay, r.UnitResolved)

	if !opts.Execute {
		return r, nil
	}
	if err := ensureMutationIdle(ctx, opts.ActivityFn, opts.IdleProbeDelay); err != nil {
		r.Err = err
		return r, nil
	}

	// 3. Stop service
	if _, err := opts.RunFn(ctx, "sudo", "systemctl", "stop", r.UnitResolved); err != nil {
		r.Err = fmt.Errorf("systemctl stop: %w", err)
		return r, nil
	}
	r.StoppedOK = true

	// 4. Download tarball
	if _, err := opts.RunFn(ctx, "curl", "-fsSL", "-o", tarPath, tarball); err != nil {
		// Try restart even on download fail (rollback to old version)
		_, _ = opts.RunFn(ctx, "sudo", "systemctl", "start", r.UnitResolved)
		r.Err = fmt.Errorf("curl tarball: %w", err)
		return r, nil
	}
	r.DownloadedOK = true

	// 5. Verify tarball before mutating runner binaries
	actual, err := opts.SHA256FileFn(tarPath)
	if err != nil {
		_, _ = opts.RunFn(ctx, "sudo", "systemctl", "start", r.UnitResolved)
		r.Err = fmt.Errorf("sha256 tarball: %w", err)
		return r, nil
	}
	if err := civm.VerifySHA256(actual, opts.RunnerSHA256, "actions/runner v"+opts.NewVersion); err != nil {
		_, _ = opts.RunFn(ctx, "sudo", "systemctl", "start", r.UnitResolved)
		r.Err = err
		return r, nil
	}

	// 6. Extract over existing (binaries + scripts; state files preserved)
	if _, err := opts.RunFn(ctx, "tar", "-C", r.Dir, "-xzf", tarPath); err != nil {
		_, _ = opts.RunFn(ctx, "sudo", "systemctl", "start", r.UnitResolved)
		r.Err = fmt.Errorf("tar extract: %w", err)
		return r, nil
	}
	r.ExtractedOK = true
	_, _ = opts.RunFn(ctx, "rm", tarPath) // best-effort cleanup

	// 7. Start service
	if _, err := opts.RunFn(ctx, "sudo", "systemctl", "start", r.UnitResolved); err != nil {
		r.Err = fmt.Errorf("systemctl start: %w", err)
		return r, nil
	}
	r.StartedOK = true

	// 8. Verify is-active
	opts.SleepFn(opts.VerifyDelay)
	out, _ := opts.RunFn(ctx, "systemctl", "is-active", r.UnitResolved)
	r.ActiveAfter = strings.TrimSpace(string(out)) == "active"
	if !r.ActiveAfter {
		r.Err = fmt.Errorf("runner nao voltou active apos upgrade (got %q)", strings.TrimSpace(string(out)))
	}
	return r, nil
}

func validateUpgradeOptions(opts UpgradeOptions) error {
	if opts.Short == "" && opts.Unit == "" {
		return fmt.Errorf("--short ou --unit obrigatorio")
	}
	if opts.Short != "" {
		if err := civm.ValidateShort(opts.Short); err != nil {
			return err
		}
	}
	if opts.Unit != "" {
		if err := civm.ValidateServiceUnit(opts.Unit); err != nil {
			return err
		}
	}
	if err := civm.ValidateSemver(opts.NewVersion, "--new-version"); err != nil {
		return err
	}
	if strings.TrimSpace(opts.RunnerSHA256) == "" {
		return fmt.Errorf("--runner-sha256 obrigatorio para actions/runner v%s", opts.NewVersion)
	}
	cleanBase, err := civm.CleanDir(opts.BaseDir, "--base-dir")
	if err != nil {
		return err
	}
	if cleanBase != opts.BaseDir {
		return fmt.Errorf("--base-dir deve estar normalizado, got %q want %q", opts.BaseDir, cleanBase)
	}
	if opts.Dir != "" {
		cleanDir, err := civm.CleanDir(opts.Dir, "--dir")
		if err != nil {
			return err
		}
		if cleanDir != opts.Dir {
			return fmt.Errorf("--dir deve estar normalizado, got %q want %q", opts.Dir, cleanDir)
		}
	}
	return nil
}

func guessDirFromShort(baseDir, short string) string {
	if short == "" {
		return baseDir
	}
	return baseDir + "/actions-runner-" + short
}

// RenderUpgradeTable writes a brief summary.
func RenderUpgradeTable(r UpgradeResult, opts UpgradeOptions, w io.Writer) {
	mode := "DRY-RUN"
	if opts.Execute {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "Modo: %s\n", mode)
	fmt.Fprintf(w, "Unit: %s | Dir: %s | Nova versao: %s\n", r.UnitResolved, r.Dir, r.NewVersion)
	if r.Err != nil {
		fmt.Fprintf(w, "Erro: %s\n", r.Err)
	}
	if !opts.Execute {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Sequencia (seria-aplicado):")
		fmt.Fprintln(w, " ", r.WouldDo)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Para aplicar: rode novamente com --execute")
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Resultado:")
	fmt.Fprintf(w, "  Stopped:    %v\n", r.StoppedOK)
	fmt.Fprintf(w, "  Downloaded: %v\n", r.DownloadedOK)
	fmt.Fprintf(w, "  Extracted:  %v\n", r.ExtractedOK)
	fmt.Fprintf(w, "  Started:    %v\n", r.StartedOK)
	fmt.Fprintf(w, "  Active:     %v\n", r.ActiveAfter)
	if r.StartedOK && r.ActiveAfter {
		fmt.Fprintln(w, "OK — upgrade aplicado, runner voltou online.")
	}
}
