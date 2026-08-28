// Package peerstatus consolida billing-status + runner online + last
// workflow run em uma view única por peer-repo. Útil pra dashboards
// rápidos sem invocar 3 ferramentas separadas. Stdlib-only.
package peerstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/emersonbusson/civm/internal/billing"
	"github.com/emersonbusson/civm/internal/civm"
)

// Status agrega informação operacional de um peer-repo.
type Status struct {
	Repo            string      `json:"repo"`
	WorkflowFile    string      `json:"workflow_file"`
	BillingStatus   string      `json:"billing_status"` // ok/blocked/unknown
	BillingExitCode int         `json:"billing_exit_code"`
	RunnersTotal    int         `json:"runners_total"`
	RunnersOnline   int         `json:"runners_online"`
	RunnerNames     []string    `json:"runner_names"`
	LastRun         *RunSummary `json:"last_run,omitempty"`
}

type Severity string

const (
	SeverityOK       Severity = "ok"
	SeverityWarn     Severity = "warn"
	SeverityCritical Severity = "critical"
)

func (s Severity) ExitCode() int {
	switch s {
	case SeverityOK:
		return 0
	case SeverityWarn:
		return 1
	default:
		return 2
	}
}

// RunSummary é o último workflow run.
type RunSummary struct {
	DatabaseID int64     `json:"database_id"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
	URL        string    `json:"url"`
}

// Options control the check.
type Options struct {
	Repo         string
	WorkflowFile string // ex: "ci.yml"
	RunFn        func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type FleetOptions struct {
	Repos        []string
	WorkflowFile string
	RunFn        func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type FleetPeer struct {
	Status
	Severity Severity `json:"severity"`
}

type FleetSummary struct {
	Total         int `json:"total"`
	OK            int `json:"ok"`
	Warn          int `json:"warn"`
	Critical      int `json:"critical"`
	RunnersOnline int `json:"runners_online"`
	RunnersTotal  int `json:"runners_total"`
}

type FleetReport struct {
	WorkflowFile string       `json:"workflow_file"`
	Summary      FleetSummary `json:"summary"`
	Peers        []FleetPeer  `json:"peers"`
	Exit         int          `json:"exit"`
}

// DefaultOptions returns sane defaults.
func DefaultOptions() Options {
	return Options{
		WorkflowFile: "ci.yml",
		RunFn:        defaultRun,
	}
}

func DefaultFleetOptions() FleetOptions {
	return FleetOptions{
		WorkflowFile: "ci.yml",
		RunFn:        defaultRun,
	}
}

// Collect consolida billing + runners + last run para o peer-repo opts.Repo.
func Collect(ctx context.Context, opts Options) (Status, error) {
	if err := validateOptions(opts); err != nil {
		return Status{}, err
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	s := Status{Repo: opts.Repo, WorkflowFile: opts.WorkflowFile}

	// 1. billing-status (delega ao package billing)
	bOpts := billing.DefaultOptions()
	bOpts.Repo = opts.Repo
	bOpts.WorkflowFile = opts.WorkflowFile
	bOpts.RunFn = opts.RunFn
	bStatus, _, _ := billing.Detect(ctx, bOpts)
	s.BillingStatus = string(bStatus)
	s.BillingExitCode = bStatus.ExitCode()

	// 2. runners online (gh api)
	type runnerJSON struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	type runnersResp struct {
		Runners []runnerJSON `json:"runners"`
	}
	out, err := opts.RunFn(ctx, "gh", "api",
		fmt.Sprintf("/repos/%s/actions/runners", opts.Repo))
	if err == nil {
		var rr runnersResp
		if json.Unmarshal(out, &rr) == nil {
			s.RunnersTotal = len(rr.Runners)
			for _, r := range rr.Runners {
				s.RunnerNames = append(s.RunnerNames, r.Name)
				if r.Status == "online" {
					s.RunnersOnline++
				}
			}
		}
	}

	// 3. last workflow run
	type runJSON struct {
		DatabaseID int64     `json:"databaseId"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		CreatedAt  time.Time `json:"createdAt"`
		URL        string    `json:"url"`
	}
	out, err = opts.RunFn(ctx, "gh", "run", "list",
		"--repo", opts.Repo,
		"--workflow", opts.WorkflowFile,
		"--limit", "1",
		"--json", "databaseId,status,conclusion,createdAt,url")
	if err == nil {
		var runs []runJSON
		if json.Unmarshal(out, &runs) == nil && len(runs) > 0 {
			r := runs[0]
			s.LastRun = &RunSummary{
				DatabaseID: r.DatabaseID,
				Status:     r.Status,
				Conclusion: r.Conclusion,
				CreatedAt:  r.CreatedAt,
				URL:        r.URL,
			}
		}
	}

	return s, nil
}

func CollectFleet(ctx context.Context, opts FleetOptions) (FleetReport, error) {
	if opts.WorkflowFile == "" {
		opts.WorkflowFile = "ci.yml"
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if err := validateFleetOptions(opts); err != nil {
		return FleetReport{}, err
	}

	report := FleetReport{
		WorkflowFile: opts.WorkflowFile,
		Peers:        []FleetPeer{},
	}
	for _, repo := range opts.Repos {
		status, err := Collect(ctx, Options{
			Repo:         repo,
			WorkflowFile: opts.WorkflowFile,
			RunFn:        opts.RunFn,
		})
		if err != nil {
			return FleetReport{}, err
		}
		report.add(status)
	}
	return report, nil
}

func validateOptions(opts Options) error {
	if err := civm.ValidateRepo(opts.Repo); err != nil {
		return err
	}
	return civm.ValidateWorkflowFile(opts.WorkflowFile)
}

func validateFleetOptions(opts FleetOptions) error {
	if len(opts.Repos) == 0 {
		return fmt.Errorf("--repos deve informar pelo menos um repo")
	}
	for _, repo := range opts.Repos {
		if err := civm.ValidateRepo(repo); err != nil {
			return err
		}
	}
	return civm.ValidateWorkflowFile(opts.WorkflowFile)
}

func (r *FleetReport) add(s Status) {
	severity := s.Severity()
	r.Peers = append(r.Peers, FleetPeer{Status: s, Severity: severity})
	r.Summary.Total++
	r.Summary.RunnersOnline += s.RunnersOnline
	r.Summary.RunnersTotal += s.RunnersTotal
	switch severity {
	case SeverityOK:
		r.Summary.OK++
	case SeverityWarn:
		r.Summary.Warn++
	default:
		r.Summary.Critical++
	}
	if code := severity.ExitCode(); code > r.Exit {
		r.Exit = code
	}
}

func (s Status) Severity() Severity {
	switch {
	case s.BillingStatus == "blocked" && s.RunnersOnline == 0:
		return SeverityCritical
	case s.BillingStatus == "blocked", s.BillingStatus == "unknown", s.RunnersOnline == 0:
		return SeverityWarn
	default:
		return SeverityOK
	}
}

// Render writes a human-readable summary.
func (s Status) Render(w io.Writer) {
	fmt.Fprintf(w, "Peer:        %s\n", s.Repo)
	fmt.Fprintf(w, "Workflow:    %s\n", s.WorkflowFile)
	fmt.Fprintf(w, "Billing:     %s (exit %d)\n", s.BillingStatus, s.BillingExitCode)
	fmt.Fprintf(w, "Runners:     %d/%d online\n", s.RunnersOnline, s.RunnersTotal)
	for _, n := range s.RunnerNames {
		fmt.Fprintf(w, "             - %s\n", n)
	}
	if s.LastRun != nil {
		conc := s.LastRun.Conclusion
		if conc == "" {
			conc = s.LastRun.Status
		}
		age := time.Since(s.LastRun.CreatedAt).Round(time.Minute)
		fmt.Fprintf(w, "Last run:    #%d %s (%s ago)\n", s.LastRun.DatabaseID, conc, age)
		fmt.Fprintf(w, "             %s\n", s.LastRun.URL)
	} else {
		fmt.Fprintln(w, "Last run:    (nenhum encontrado)")
	}
	fmt.Fprintln(w)
	switch {
	case s.BillingStatus == "blocked" && s.RunnersOnline == 0:
		fmt.Fprintln(w, "ALERTA: billing-block + nenhum runner self-hosted online = workflow nao roda.")
	case s.BillingStatus == "blocked" && s.RunnersOnline > 0:
		fmt.Fprintln(w, "WARN: billing-block detectado, mas civm self-hosted serve fallback.")
	case s.BillingStatus == "unknown":
		fmt.Fprintln(w, "WARN: billing status desconhecido; confirmar gh/auth/runs antes de concluir saude.")
	case s.RunnersOnline == 0:
		fmt.Fprintln(w, "WARN: nenhum runner self-hosted online; depende 100 percent de billing-hosted.")
	default:
		fmt.Fprintln(w, "OK: billing OK + runners online.")
	}
}

// RenderJSON writes machine-readable output.
func (s Status) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func (r FleetReport) Render(w io.Writer) {
	fmt.Fprintf(w, "civm peer-status | workflow=%s | repos=%d | exit=%d\n",
		r.WorkflowFile, r.Summary.Total, r.Exit)
	fmt.Fprintln(w, "Modo: observabilidade read-only; civmctl nao corrige peers automaticamente.")
	fmt.Fprintf(w, "Resumo: ok=%d warn=%d critical=%d runners=%d/%d online\n\n",
		r.Summary.OK, r.Summary.Warn, r.Summary.Critical,
		r.Summary.RunnersOnline, r.Summary.RunnersTotal)

	fmt.Fprintf(w, "%-24s %-8s %-10s %-11s %-22s %s\n",
		"REPO", "SEVERITY", "BILLING", "RUNNERS", "LAST RUN", "URL")
	for _, p := range r.Peers {
		lastRun := "(nenhum)"
		lastURL := ""
		if p.LastRun != nil {
			conclusion := p.LastRun.Conclusion
			if conclusion == "" {
				conclusion = p.LastRun.Status
			}
			lastRun = fmt.Sprintf("#%d %s", p.LastRun.DatabaseID, conclusion)
			lastURL = p.LastRun.URL
		}
		fmt.Fprintf(w, "%-24s %-8s %-10s %3d/%-7d %-22s %s\n",
			truncate(p.Repo, 24), p.Severity, p.BillingStatus,
			p.RunnersOnline, p.RunnersTotal, truncate(lastRun, 22), lastURL)
	}

	fmt.Fprintln(w)
	switch {
	case r.Summary.Critical > 0:
		fmt.Fprintln(w, "ALERTA: pelo menos um peer esta critical; CI pode nao rodar sem intervencao manual.")
	case r.Summary.Warn > 0:
		fmt.Fprintln(w, "WARN: pelo menos um peer exige verificacao antes de publicar ou investigar CI.")
	default:
		fmt.Fprintln(w, "OK: todos os peers analisados estao operacionais.")
	}
}

func (r FleetReport) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
