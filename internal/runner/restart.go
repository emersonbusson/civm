package runner

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/idle"
)

// RestartOptions controls a runner restart.
type RestartOptions struct {
	Short          string        // suffix curto (ex: cmpx, peer, acme)
	Unit           string        // explicit systemd unit name (sobreescreve Short)
	VerifyDelay    time.Duration // sleep entre restart e is-active check (default 3s)
	IdleProbeDelay time.Duration
	Execute        bool // false = dry-run (apenas resolve unit name)
	RunFn          func(ctx context.Context, name string, args ...string) ([]byte, error)
	ActivityFn     func(ctx context.Context) ([]idle.Activity, error)
	SleepFn        func(d time.Duration)
}

// DefaultRestartOptions returns sane defaults.
func DefaultRestartOptions() RestartOptions {
	return RestartOptions{
		VerifyDelay:    time.Duration(civm.DefaultRestartVerifySeconds) * time.Second,
		IdleProbeDelay: 2 * time.Second,
		Execute:        false,
		RunFn:          defaultRun,
		ActivityFn:     idle.DefaultActivities,
		SleepFn:        time.Sleep,
	}
}

// RestartResult captures the restart outcome.
type RestartResult struct {
	UnitResolved string // unit name resolvido a partir de Short
	RestartedOK  bool   // true se systemctl restart retornou sucesso
	ActiveAfter  bool   // true se systemctl is-active retornou active após delay
	WouldDo      string // descricao em dry-run
	Err          error
}

// Restart resolves the unit name from Short (ou usa Unit explícito),
// chama systemctl restart e verifica is-active após VerifyDelay.
//
// Resolution: lista todas units actions.runner.* e filtra por unit
// que CONTÉM .Short.. Erro se 0 ou >1 match (ambiguidade).
func Restart(ctx context.Context, opts RestartOptions) (RestartResult, error) {
	if err := validateRestartOptions(opts); err != nil {
		return RestartResult{}, err
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.ActivityFn == nil {
		opts.ActivityFn = idle.DefaultActivities
	}
	if opts.SleepFn == nil {
		opts.SleepFn = time.Sleep
	}
	if opts.VerifyDelay == 0 {
		opts.VerifyDelay = time.Duration(civm.DefaultRestartVerifySeconds) * time.Second
	}
	r := RestartResult{}

	if opts.Unit != "" {
		r.UnitResolved = opts.Unit
	} else {
		unit, err := resolveUnitByShort(ctx, opts)
		if err != nil {
			r.Err = err
			return r, nil
		}
		r.UnitResolved = unit
	}

	r.WouldDo = fmt.Sprintf("sudo systemctl restart %s && sleep %s && systemctl is-active %s",
		r.UnitResolved, opts.VerifyDelay, r.UnitResolved)

	if !opts.Execute {
		return r, nil
	}
	if err := ensureMutationIdle(ctx, opts.ActivityFn, opts.IdleProbeDelay); err != nil {
		r.Err = err
		return r, nil
	}

	// systemctl restart
	if _, err := opts.RunFn(ctx, "sudo", "systemctl", "restart", r.UnitResolved); err != nil {
		r.Err = fmt.Errorf("systemctl restart: %w", err)
		return r, nil
	}
	r.RestartedOK = true

	opts.SleepFn(opts.VerifyDelay)

	// systemctl is-active
	out, _ := opts.RunFn(ctx, "systemctl", "is-active", r.UnitResolved)
	r.ActiveAfter = strings.TrimSpace(string(out)) == "active"
	if !r.ActiveAfter {
		r.Err = fmt.Errorf("runner nao voltou active apos %s (got %q)",
			opts.VerifyDelay, strings.TrimSpace(string(out)))
	}
	return r, nil
}

func validateRestartOptions(opts RestartOptions) error {
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
	return nil
}

// resolveUnitByShort lista actions.runner.* e filtra units que contem
// ".<Short>." OR ".<Short>.service" OR terminam com "<Short>.service".
// Erro se 0 ou >1 match.
func resolveUnitByShort(ctx context.Context, opts RestartOptions) (string, error) {
	listOpts := DefaultListOptions()
	listOpts.RunFn = opts.RunFn
	items, err := List(ctx, listOpts)
	if err != nil {
		return "", fmt.Errorf("list units: %w", err)
	}
	var matches []string
	for _, s := range items {
		if s.Name == opts.Short || strings.Contains(s.UnitName, "."+opts.Short+".") {
			matches = append(matches, s.UnitName)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("nenhum runner com short=%q (rodar civmctl runner list pra ver disponiveis)", opts.Short)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("short=%q ambiguo, %d matches: %s (use --unit pra especificar)",
			opts.Short, len(matches), strings.Join(matches, ", "))
	}
	return matches[0], nil
}

// RenderRestartTable writes a brief summary.
func RenderRestartTable(r RestartResult, opts RestartOptions, w io.Writer) {
	mode := "DRY-RUN"
	if opts.Execute {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "Modo: %s\n", mode)
	fmt.Fprintf(w, "Unit resolvido: %s\n", r.UnitResolved)
	if r.Err != nil {
		fmt.Fprintf(w, "Erro: %s\n", r.Err)
		return
	}
	if !opts.Execute {
		fmt.Fprintf(w, "(seria-aplicado): %s\n", r.WouldDo)
		fmt.Fprintln(w, "Para aplicar: rode novamente com --execute")
		return
	}
	fmt.Fprintf(w, "Restart: %v | Active apos delay: %v\n", r.RestartedOK, r.ActiveAfter)
	if r.RestartedOK && r.ActiveAfter {
		fmt.Fprintln(w, "OK — runner voltou online sem perder job em curso (best-effort).")
	}
}
