// Package billing implements the GitHub Actions billing-block heuristic
// detector. Validated against the 2026-05-09 billing-block incident.
// Stdlib-only.
package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
)

// Status classifies the GitHub Actions billing state.
type Status string

const (
	// StatusOK — recent runs executed normally (>=1 com duration significativa
	// OU conclusao success/cancelled).
	StatusOK Status = "ok"
	// StatusBlocked — 3+ runs consecutivos finalizados em <10s com
	// conclusion failure: padrao de billing block (job nao iniciou).
	StatusBlocked Status = "blocked"
	// StatusUnknown — nao foi possivel inspecionar (gh ausente, sem
	// runs ainda, parse falhou).
	StatusUnknown Status = "unknown"
)

// ExitCode mapeia Status → sysexits-like code (0=ok, 1=blocked, 2=unknown).
func (s Status) ExitCode() int {
	switch s {
	case StatusOK:
		return 0
	case StatusBlocked:
		return 1
	}
	return 2
}

// Run espelha o JSON de `gh run list --json databaseId,status,conclusion,startedAt,updatedAt`.
type Run struct {
	DatabaseID int64     `json:"databaseId"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Options control how the heuristic runs.
type Options struct {
	Repo         string        // "owner/repo"; passed via gh --repo
	WorkflowFile string        // ex: "ci.yml" (default)
	Limit        int           // numero de runs a fetchar (default 5)
	Threshold    time.Duration // duracao maxima pra considerar "morto cedo" (default 10s)
	MinBlocked   int           // min consecutive blocked runs pra StatusBlocked (default 3)
	RunFn        func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultOptions returns sane defaults.
func DefaultOptions() Options {
	return Options{
		WorkflowFile: "ci.yml",
		Limit:        5,
		Threshold:    10 * time.Second,
		MinBlocked:   3,
		RunFn:        defaultRun,
	}
}

// Detect inspects the most recent runs of opts.WorkflowFile in opts.Repo
// and returns Status + raw runs (for inspection).
func Detect(ctx context.Context, opts Options) (Status, []Run, error) {
	if err := validateOptions(opts); err != nil {
		return StatusUnknown, nil, err
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	args := []string{
		"run", "list",
		"--repo", opts.Repo,
		"--workflow", opts.WorkflowFile,
		"--limit", fmt.Sprintf("%d", opts.Limit),
		"--json", "databaseId,status,conclusion,startedAt,updatedAt",
	}
	out, err := opts.RunFn(ctx, "gh", args...)
	if err != nil {
		return StatusUnknown, nil, fmt.Errorf("gh run list: %w", err)
	}
	var runs []Run
	if err := json.Unmarshal(out, &runs); err != nil {
		return StatusUnknown, nil, fmt.Errorf("parse gh output: %w", err)
	}
	return classifyRuns(runs, opts), runs, nil
}

// classifyRuns aplica a heuristica em runs ordenados (mais recente primeiro).
// Considera apenas runs com startedAt nao-zero (run efetivamente iniciado).
func classifyRuns(runs []Run, opts Options) Status {
	considered := make([]Run, 0, len(runs))
	for _, r := range runs {
		if r.StartedAt.IsZero() {
			continue
		}
		considered = append(considered, r)
		if len(considered) == opts.MinBlocked {
			break
		}
	}
	if len(considered) < opts.MinBlocked {
		return StatusUnknown
	}
	blockedCount := 0
	for _, r := range considered {
		if r.Conclusion != "failure" {
			return StatusOK
		}
		duration := r.UpdatedAt.Sub(r.StartedAt)
		if duration < opts.Threshold {
			blockedCount++
		} else {
			return StatusOK
		}
	}
	if blockedCount >= opts.MinBlocked {
		return StatusBlocked
	}
	return StatusOK
}

func validateOptions(opts Options) error {
	if err := civm.ValidateRepo(opts.Repo); err != nil {
		return err
	}
	if err := civm.ValidateWorkflowFile(opts.WorkflowFile); err != nil {
		return err
	}
	if opts.Limit < 3 {
		return fmt.Errorf("--limit deve ser >= 3, got %d", opts.Limit)
	}
	if opts.MinBlocked < 1 {
		return fmt.Errorf("--min-blocked deve ser >= 1, got %d", opts.MinBlocked)
	}
	return nil
}

// Render writes a human-readable status report.
func Render(status Status, runs []Run, opts Options, w io.Writer) {
	fmt.Fprintf(w, "Repo: %s | Workflow: %s | Status: %s (exit %d)\n",
		opts.Repo, opts.WorkflowFile, status, status.ExitCode())
	_, _ = fmt.Fprintln(w)
	switch status {
	case StatusOK:
		fmt.Fprintln(w, "[billing] ok — runs recentes executando normalmente")
	case StatusBlocked:
		fmt.Fprintln(w, "[billing] blocked — 3+ runs consecutivos finalizados em <10s")
		fmt.Fprintln(w, "[billing]   causa provavel: spending limit ou falha de pagamento")
		fmt.Fprintln(w, "[billing]   acao: GitHub Settings > Billing & plans")
		fmt.Fprintln(w, "[billing]   fallback: civm self-hosted runner pega jobs com label civm")
	case StatusUnknown:
		fmt.Fprintln(w, "[billing] unknown — nao foi possivel inspecionar")
		fmt.Fprintln(w, "[billing]   causa provavel: gh ausente, sem runs, parse falhou ou auth")
		fmt.Fprintln(w, "[billing]   acao local: 'gh auth login' uma vez")
		fmt.Fprintln(w, "[billing]   acao workflow: confirmar GH_TOKEN: ${{ secrets.GITHUB_TOKEN }} no env")
	}
	if len(runs) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Ultimos runs analisados (limit=%d):\n", opts.Limit)
		for i, r := range runs {
			if i >= opts.MinBlocked && status != StatusUnknown {
				break
			}
			dur := "?"
			if !r.StartedAt.IsZero() && !r.UpdatedAt.IsZero() {
				dur = r.UpdatedAt.Sub(r.StartedAt).Round(time.Second).String()
			}
			fmt.Fprintf(w, "  [%d] id=%d conclusion=%s duration=%s\n",
				i+1, r.DatabaseID, r.Conclusion, dur)
		}
	}
}

// RenderJSON emits machine-readable output.
func RenderJSON(status Status, runs []Run, opts Options, w io.Writer) error {
	type out struct {
		Repo         string `json:"repo"`
		WorkflowFile string `json:"workflow_file"`
		Status       string `json:"status"`
		ExitCode     int    `json:"exit_code"`
		RunsAnalyzed []Run  `json:"runs_analyzed"`
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out{
		Repo:         opts.Repo,
		WorkflowFile: opts.WorkflowFile,
		Status:       string(status),
		ExitCode:     status.ExitCode(),
		RunsAnalyzed: runs,
	})
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
