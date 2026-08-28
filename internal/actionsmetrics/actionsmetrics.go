// Package actionsmetrics agrega GitHub Actions usage (minutos + job
// runs) cross-repo num período, espelhando o painel "Actions Usage
// Metrics" do GitHub. Stdlib-only; concorrência via worker pool.
//
// Limites do que cobre nesta versão:
//
//   - Minutos billable (hosted runners) vêm exatos da API
//     /organizations/{org}/settings/billing/usage.
//   - Run counts vêm de /repos/{owner}/{repo}/actions/runs?created=...
//     com paginação.
//   - Self-hosted minutos NÃO são fornecidos pela API pública diretamente
//     — esta versão reporta `null` em `self_hosted_minutes` e o consumidor
//     pode estimar via job-level breakdown (próxima iteração).
package actionsmetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/runner"
)

// UsageItem é um line item retornado pela billing API. Note: a API
// publica retorna camelCase (unitType, organizationName, repositoryName).
type UsageItem struct {
	Date             time.Time `json:"date"`
	Product          string    `json:"product"`
	SKU              string    `json:"sku"`
	Quantity         float64   `json:"quantity"`
	UnitType         string    `json:"unitType"`
	OrganizationName string    `json:"organizationName"`
	RepositoryName   string    `json:"repositoryName"`
}

// RepoMetric é o agregado por repo do fleet.
type RepoMetric struct {
	Repo           string `json:"repo"`
	HostedMinutes  int64  `json:"hosted_minutes"`
	RunsTotal      int    `json:"runs_total"`
	RunsInPeriod   int    `json:"runs_in_period"`
	StorageGBHours int64  `json:"storage_gb_hours_x1000"` // GB-hours × 1000 (sub-unit precision)
}

// SKUTotal é o agregado por SKU (Actions Linux/macOS/Windows/storage).
type SKUTotal struct {
	SKU      string  `json:"sku"`
	Minutes  int64   `json:"minutes,omitempty"`
	Storage  float64 `json:"storage_gb_hours,omitempty"`
	UnitType string  `json:"unit_type"`
}

// Report é a saída agregada.
type Report struct {
	CollectedAt  time.Time    `json:"collected_at"`
	StartDate    string       `json:"start_date"`
	EndDate      string       `json:"end_date"`
	Organization string       `json:"organization"`
	TotalMinutes int64        `json:"total_minutes"` // soma de Actions minutes (billable)
	TotalRuns    int          `json:"total_runs"`    // count cross-repo
	BySKU        []SKUTotal   `json:"by_sku"`
	ByRepo       []RepoMetric `json:"by_repo"`
	Exit         int          `json:"exit"`
}

// Options controla a coleta.
type Options struct {
	Organization    string   // ex: "acme"
	Repos           []string // owner/repo; vazio + InferRepos descobre via systemd
	InferRepos      bool
	StartDate       string // ISO YYYY-MM-DD
	EndDate         string // ISO YYYY-MM-DD
	Concurrency     int
	Now             func() time.Time
	RunFn           func(ctx context.Context, name string, args ...string) ([]byte, error)
	SystemRunnersFn func(ctx context.Context) ([]runner.Status, error)
}

// DefaultOptions retorna defaults conservadores. Período = mês atual.
func DefaultOptions() Options {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return Options{
		StartDate:   start.Format("2006-01-02"),
		EndDate:     now.Format("2006-01-02"),
		Concurrency: 4,
		Now:         time.Now,
		RunFn:       defaultRun,
	}
}

// Collect agrega billing usage + run counts pro período/org/repos.
//
// Fluxo:
//
//  1. gh api /organizations/{org}/settings/billing/usage?start_date=...&end_date=...
//     → minutos hosted + storage por (date, sku, repo).
//  2. Pra cada repo, gh api /repos/{O}/{R}/actions/runs?created=START..END&per_page=1
//     pra extrair `total_count` (gh APIs retornam header X-Total-Count
//     mas a contagem `total_count` no body é confiável).
//  3. Soma cross-repo + breakdown por SKU e por repo.
//
// Self-hosted minutos NÃO entram aqui — o campo fica fora desta versão
// porque a API pública não expõe (cada job teria que ser iterado pra
// medir, custo proibitivo num cycle de cockpit).
func Collect(ctx context.Context, opts Options) (Report, error) {
	applyDefaults(&opts)
	if err := validateOptions(opts); err != nil {
		return Report{}, err
	}

	repos, err := resolveRepos(ctx, &opts)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		CollectedAt:  opts.Now().UTC(),
		StartDate:    opts.StartDate,
		EndDate:      opts.EndDate,
		Organization: opts.Organization,
		BySKU:        []SKUTotal{},
		ByRepo:       []RepoMetric{},
	}

	// O exit code começa em 0 e sobe pra 1 se qualquer fase recuperável
	// falhar (billing ou run-count). A coleta nunca aborta por erro
	// parcial — o consumidor inspeciona report.Exit.
	exitCode := 0

	// Fase de billing: minutos hosted + storage por (sku, repo). Falha
	// aqui não interrompe — os run counts ainda podem completar.
	usage, err := fetchBillingUsage(ctx, opts)
	if err != nil {
		exitCode = 1
	}
	repoMinutes, repoStorageX1000 := aggregateSKUs(usage, &report)

	// Fase de run-count: fan-out por repo (ou agregação só-por-billing
	// quando não há repos enumerados).
	if assembleByRepo(ctx, opts, &report, repos, repoMinutes, repoStorageX1000) {
		exitCode = 1
	}
	sort.Slice(report.ByRepo, func(i, j int) bool {
		return report.ByRepo[i].HostedMinutes > report.ByRepo[j].HostedMinutes
	})

	report.Exit = exitCode
	return report, nil
}

// validateOptions checa org e período antes de qualquer chamada de rede.
// Falhar cedo evita gastar uma chamada gh com input inválido.
func validateOptions(opts Options) error {
	if !validOrg(opts.Organization) {
		return fmt.Errorf("organization deve ser slug seguro [A-Za-z0-9-]+, got %q", opts.Organization)
	}
	if !validDate(opts.StartDate) || !validDate(opts.EndDate) {
		return fmt.Errorf("start_date/end_date devem ser YYYY-MM-DD; got %q/%q", opts.StartDate, opts.EndDate)
	}
	return nil
}

// aggregateSKUs percorre os usage items de Actions e preenche report.BySKU
// + report.TotalMinutes. Retorna os mapas auxiliares minutos-por-repo e
// storage-por-repo (×1000) que a montagem do ByRepo consome depois. Só
// items com Product == "actions" entram; SKUs de outros produtos são
// ignorados. A ordenação do BySKU é por minutos desc.
func aggregateSKUs(usage []UsageItem, report *Report) (repoMinutes, repoStorageX1000 map[string]int64) {
	skuTotals := map[string]*SKUTotal{}
	repoMinutes = map[string]int64{}
	repoStorageX1000 = map[string]int64{}
	for _, it := range usage {
		if it.Product != "actions" {
			continue
		}
		t, ok := skuTotals[it.SKU]
		if !ok {
			t = &SKUTotal{SKU: it.SKU, UnitType: it.UnitType}
			skuTotals[it.SKU] = t
		}
		switch it.UnitType {
		case "Minutes":
			minutes := int64(it.Quantity)
			t.Minutes += minutes
			report.TotalMinutes += minutes
			if it.RepositoryName != "" {
				repoMinutes[it.RepositoryName] += minutes
			}
		case "GigabyteHours":
			t.Storage += it.Quantity
			if it.RepositoryName != "" {
				repoStorageX1000[it.RepositoryName] += int64(it.Quantity * 1000)
			}
		}
	}
	for _, t := range skuTotals {
		report.BySKU = append(report.BySKU, *t)
	}
	sort.Slice(report.BySKU, func(i, j int) bool { return report.BySKU[i].Minutes > report.BySKU[j].Minutes })
	return repoMinutes, repoStorageX1000
}

// assembleByRepo preenche report.ByRepo e report.TotalRuns. Com repos
// enumerados, faz fan-out de run-count (um gh por repo) e cruza com os
// minutos/storage do billing; sem repos, agrega só pelo que o billing
// reportou. Devolve true se algum fetch de run-count falhou (sinaliza
// exit recuperável ao caller).
func assembleByRepo(ctx context.Context, opts Options, report *Report, repos []string, repoMinutes, repoStorageX1000 map[string]int64) bool {
	if len(repos) == 0 {
		// Sem repos enumerados: agrega-só por billing.
		for repoName, m := range repoMinutes {
			report.ByRepo = append(report.ByRepo, RepoMetric{
				Repo:           opts.Organization + "/" + repoName,
				HostedMinutes:  m,
				StorageGBHours: repoStorageX1000[repoName],
			})
		}
		return false
	}

	repoRuns, failed := fetchRunCounts(ctx, opts, repos)
	for _, repo := range repos {
		parts := strings.SplitN(repo, "/", 2)
		repoName := parts[len(parts)-1]
		report.ByRepo = append(report.ByRepo, RepoMetric{
			Repo:           repo,
			HostedMinutes:  repoMinutes[repoName],
			RunsTotal:      repoRuns[repo],
			RunsInPeriod:   repoRuns[repo],
			StorageGBHours: repoStorageX1000[repoName],
		})
		report.TotalRuns += repoRuns[repo]
	}
	return failed
}

// fetchRunCounts dispara um fetchRunCount por repo num worker pool e
// devolve o mapa repo→count. O segundo retorno é true se ao menos um
// fetch falhou — o caller mapeia isso pro exit code recuperável.
func fetchRunCounts(ctx context.Context, opts Options, repos []string) (map[string]int, bool) {
	type runCountResult struct {
		repo  string
		count int
		err   error
	}
	results := make([]runCountResult, len(repos))
	runJobs(ctx, opts.Concurrency, len(repos), func(i int) {
		c, err := fetchRunCount(ctx, opts, repos[i])
		results[i] = runCountResult{repo: repos[i], count: c, err: err}
	})
	repoRuns := make(map[string]int, len(repos))
	failed := false
	for _, r := range results {
		if r.err != nil {
			failed = true
		}
		repoRuns[r.repo] = r.count
	}
	return repoRuns, failed
}

// resolveRepos devolve a lista final de repos a coletar. Usa opts.Repos
// quando preenchida; caso contrário, com InferRepos, descobre os repos a
// partir dos runners systemd (injetando runner.List como fonte default).
// A lista resultante é validada antes de retornar.
//
// Este é o prelúdio de repo-inference compartilhado com o pacote
// activeruns; cada pacote mantém o seu próprio inferReposFromSystemd
// porque o filtro de shape difere (aqui basta o repo conter "/").
func resolveRepos(ctx context.Context, opts *Options) ([]string, error) {
	repos := append([]string(nil), opts.Repos...)
	if opts.InferRepos && len(repos) == 0 {
		if opts.SystemRunnersFn == nil {
			opts.SystemRunnersFn = func(ctx context.Context) ([]runner.Status, error) {
				lo := runner.DefaultListOptions()
				lo.RunFn = opts.RunFn
				return runner.List(ctx, lo)
			}
		}
		systemd, err := opts.SystemRunnersFn(ctx)
		if err != nil {
			return nil, fmt.Errorf("infer repos: %w", err)
		}
		repos = inferReposFromSystemd(systemd)
	}
	if err := validateRepos(repos); err != nil {
		return nil, err
	}
	return repos, nil
}

// fetchBillingUsage chama /organizations/{org}/settings/billing/usage com
// filtro de data. Retorna os items raw — já dá pra agregar por SKU/repo.
func fetchBillingUsage(ctx context.Context, opts Options) ([]UsageItem, error) {
	endpoint := fmt.Sprintf(
		"organizations/%s/settings/billing/usage?start_date=%s&end_date=%s",
		opts.Organization, opts.StartDate, opts.EndDate,
	)
	out, err := opts.RunFn(ctx, "gh", "api", endpoint)
	if err != nil {
		return nil, fmt.Errorf("gh api billing/usage: %w", err)
	}
	var body struct {
		UsageItems []UsageItem `json:"usageItems"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		return nil, fmt.Errorf("parse billing/usage: %w", err)
	}
	return body.UsageItems, nil
}

// fetchRunCount usa `total_count` da listagem de runs do período via
// `per_page=1` pra evitar paginação pesada. A API retorna total exato no
// body mesmo com per_page=1.
func fetchRunCount(ctx context.Context, opts Options, repo string) (int, error) {
	endpoint := fmt.Sprintf(
		"repos/%s/actions/runs?created=%s..%s&per_page=1",
		repo, opts.StartDate, opts.EndDate,
	)
	out, err := opts.RunFn(ctx, "gh", "api", endpoint)
	if err != nil {
		return 0, fmt.Errorf("gh api runs %s: %w", repo, err)
	}
	var body struct {
		TotalCount int `json:"total_count"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		return 0, fmt.Errorf("parse runs %s: %w", repo, err)
	}
	return body.TotalCount, nil
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func inferReposFromSystemd(systemd []runner.Status) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range systemd {
		repo := s.Repo
		if repo == "" || !strings.Contains(repo, "/") {
			continue
		}
		if _, ok := seen[repo]; ok {
			continue
		}
		seen[repo] = struct{}{}
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

func validateRepos(repos []string) error {
	for _, r := range repos {
		if err := civm.ValidateRepo(r); err != nil {
			return err
		}
	}
	return nil
}

func runJobs(ctx context.Context, concurrency, n int, fn func(i int)) {
	if n == 0 {
		return
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > n {
		concurrency = n
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(idx)
		}(i)
	}
	wg.Wait()
}

func applyDefaults(opts *Options) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.StartDate == "" || opts.EndDate == "" {
		now := opts.Now().UTC()
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		if opts.StartDate == "" {
			opts.StartDate = start.Format("2006-01-02")
		}
		if opts.EndDate == "" {
			opts.EndDate = now.Format("2006-01-02")
		}
	}
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Render escreve view humana.
func (r Report) Render(w io.Writer) {
	fmt.Fprintf(w, "civm actions-metrics | org=%s | %s..%s\n",
		r.Organization, r.StartDate, r.EndDate)
	fmt.Fprintf(w, "Totais:  %d minutos billable | %d run total\n\n",
		r.TotalMinutes, r.TotalRuns)
	if len(r.BySKU) > 0 {
		fmt.Fprintln(w, "Por SKU:")
		for _, t := range r.BySKU {
			if t.UnitType == "Minutes" {
				fmt.Fprintf(w, "  %-24s %8d min\n", t.SKU, t.Minutes)
			} else {
				fmt.Fprintf(w, "  %-24s %8.4f GB-h\n", t.SKU, t.Storage)
			}
		}
		fmt.Fprintln(w)
	}
	if len(r.ByRepo) > 0 {
		fmt.Fprintf(w, "%-32s %10s %10s\n", "REPO", "MIN(hosted)", "RUNS")
		for _, m := range r.ByRepo {
			fmt.Fprintf(w, "%-32s %10d %10d\n", m.Repo, m.HostedMinutes, m.RunsTotal)
		}
	}
}

// RenderJSON escreve o report como JSON indentado.
func (r Report) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Period é um helper pra construir start/end dates de período comum.
func Period(name string, now time.Time) (start, end string, err error) {
	now = now.UTC()
	switch name {
	case "", "month", "current-month":
		s := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return s.Format("2006-01-02"), now.Format("2006-01-02"), nil
	case "last-month":
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		lastEnd := first.AddDate(0, 0, -1)
		lastStart := time.Date(lastEnd.Year(), lastEnd.Month(), 1, 0, 0, 0, 0, time.UTC)
		return lastStart.Format("2006-01-02"), lastEnd.Format("2006-01-02"), nil
	case "week", "current-week":
		// ISO week start (Monday)
		offset := int(now.Weekday()) - 1
		if offset < 0 {
			offset = 6
		}
		s := now.AddDate(0, 0, -offset)
		return s.Format("2006-01-02"), now.Format("2006-01-02"), nil
	case "day", "today":
		return now.Format("2006-01-02"), now.Format("2006-01-02"), nil
	default:
		// Try parse as range: YYYY-MM-DD..YYYY-MM-DD
		parts := strings.SplitN(name, "..", 2)
		if len(parts) == 2 && validDate(parts[0]) && validDate(parts[1]) {
			return parts[0], parts[1], nil
		}
		return "", "", fmt.Errorf("period inválido: %q (use month|last-month|week|day|YYYY-MM-DD..YYYY-MM-DD)", name)
	}
}

var orgSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

func validOrg(s string) bool {
	return s != "" && len(s) <= 64 && orgSlug.MatchString(s)
}
