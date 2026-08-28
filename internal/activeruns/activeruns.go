// Package activeruns lista workflow runs em andamento (in_progress + queued)
// agregados por repo, com ETA estimado pelo histórico das últimas N runs
// concluídas com sucesso. Stdlib-only; concorrência via worker pool.
//
// Pertence à família dos comandos read-only de cockpit (capacity, doctor,
// peer-status, health) — útil pra dashboards e scripts que precisam saber
// "o que está rodando agora" sem invocar gh manualmente por repo.
package activeruns

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/runner"
)

// Run é um workflow run em andamento ou enfileirado.
type Run struct {
	DatabaseID     int64     `json:"database_id"`
	Repo           string    `json:"repo"`
	Workflow       string    `json:"workflow"`
	Title          string    `json:"title"`
	Event          string    `json:"event"`
	Branch         string    `json:"branch"`
	Status         string    `json:"status"` // in_progress | queued
	URL            string    `json:"url"`
	CreatedAt      time.Time `json:"created_at"`
	AvgDurationSec *int64    `json:"avg_duration_sec,omitempty"`
}

// Summary agrega contadores e ETA total das runs ativas.
type Summary struct {
	InProgress  int   `json:"in_progress"`
	Queued      int   `json:"queued"`
	ETATotalSec int64 `json:"eta_total_sec"`
}

// Report é a saída read-only do comando.
type Report struct {
	CollectedAt time.Time `json:"collected_at"`
	Runs        []Run     `json:"runs"`
	Summary     Summary   `json:"summary"`
	Exit        int       `json:"exit"`
}

// Options controla a coleta.
type Options struct {
	Repos        []string // owner/repo; vazio + InferRepos descobre via systemd
	InferRepos   bool
	IncludeETA   bool
	Limit        int // por (repo, status), default 5
	HistoryLimit int // por workflow, default 10 success runs
	Concurrency  int // workers paralelos pra chamadas gh, default 8
	Now          func() time.Time
	RunFn        func(ctx context.Context, name string, args ...string) ([]byte, error)
	// SystemRunnersFn permite injetar runners simulados em teste; quando
	// nil, usa runner.List com o mesmo RunFn.
	SystemRunnersFn func(ctx context.Context) ([]runner.Status, error)
	// ReposConfigFn supplies the explicit fleet when only an org-level runner
	// exists. The default reads CIVM_REAPER_REPOS from run-reaper.env.
	ReposConfigFn func() ([]string, error)
	// OrgReposFn expands an org-level runner when the local fleet config is
	// unavailable to the caller (for example, a root-only environment file).
	OrgReposFn func(ctx context.Context, org string) ([]string, error)
}

// DefaultOptions retorna defaults conservadores.
func DefaultOptions() Options {
	return Options{
		IncludeETA:    true,
		Limit:         5,
		HistoryLimit:  10,
		Concurrency:   8,
		Now:           time.Now,
		RunFn:         defaultRun,
		ReposConfigFn: defaultReposConfig,
	}
}

// Collect lista todas as runs in_progress + queued para Repos (ou
// inferidos da systemd se InferRepos=true), opcionalmente enriquece com
// AvgDurationSec calculado das últimas HistoryLimit runs success.
//
// Erros parciais (uma chamada gh falhar) não abortam a coleta — runs das
// outras chamadas continuam no Report. Caller pode checar `Exit` (0 = OK,
// >0 = pelo menos um erro recuperado).
func Collect(ctx context.Context, opts Options) (Report, error) {
	applyDefaults(&opts)

	repos, err := resolveRepos(ctx, &opts)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		CollectedAt: opts.Now().UTC(),
		Runs:        []Run{},
	}
	if len(repos) == 0 {
		return report, nil
	}

	// Fase de listagem: fan-out (repo × status) → runs ativas + exit
	// recuperável quando alguma chamada gh falha.
	all, exitCode := fetchActiveRuns(ctx, opts, repos)

	// Fase de ETA (opcional): enriquece cada run com AvgDurationSec do
	// histórico de success do seu workflow.
	if opts.IncludeETA && len(all) > 0 {
		enrichETA(ctx, opts, all)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Status != all[j].Status {
			return all[i].Status == "in_progress"
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})

	report.Runs = all
	report.Summary = summarize(all, opts.Now())
	report.Exit = exitCode
	return report, nil
}

// resolveRepos devolve a lista final de repos a inspecionar. Usa
// opts.Repos quando preenchida; caso contrário, com InferRepos, descobre
// os repos a partir dos runners systemd (injetando runner.List como fonte
// default). A lista resultante é validada antes de retornar.
//
// Este é o prelúdio de repo-inference compartilhado com o pacote
// actionsmetrics; cada pacote mantém o seu próprio inferReposFromSystemd
// porque o filtro de shape difere (aqui exige owner/repo via regex).
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
		if len(repos) == 0 {
			repos, err = opts.ReposConfigFn()
			if err != nil {
				return nil, fmt.Errorf("infer repos from config: %w", err)
			}
		}
		if len(repos) == 0 {
			for _, org := range inferOrgsFromSystemd(systemd) {
				orgRepos, orgErr := opts.OrgReposFn(ctx, org)
				if orgErr != nil {
					return nil, fmt.Errorf("infer repos from org %s: %w", org, orgErr)
				}
				repos = append(repos, orgRepos...)
			}
		}
	}
	if err := validateRepos(repos); err != nil {
		return nil, err
	}
	return repos, nil
}

// fetchActiveRuns dispara um listRuns por (repo, status) num worker pool,
// concatena os runs e devolve o exit recuperável (1 se alguma chamada gh
// falhou). Erros parciais não abortam — runs das outras chamadas seguem
// no resultado.
func fetchActiveRuns(ctx context.Context, opts Options, repos []string) ([]Run, int) {
	type listKey struct{ repo, status string }
	type listResult struct {
		key  listKey
		runs []Run
		err  error
	}

	// Capacidade exata: cada repo gera 2 jobs (in_progress + queued).
	jobs := make([]listKey, 0, 2*len(repos))
	for _, r := range repos {
		for _, s := range []string{"in_progress", "queued"} {
			jobs = append(jobs, listKey{r, s})
		}
	}

	results := make([]listResult, len(jobs))
	runJobs(ctx, opts.Concurrency, len(jobs), func(i int) {
		k := jobs[i]
		runs, err := listRuns(ctx, opts, k.repo, k.status, opts.Limit)
		results[i] = listResult{k, runs, err}
	})

	exitCode := 0
	var all []Run
	for _, r := range results {
		if r.err != nil {
			exitCode = 1
		}
		all = append(all, r.runs...)
	}
	return all, exitCode
}

// enrichETA preenche AvgDurationSec de cada run in-place. Calcula a média
// uma vez por (repo, workflow) distinto — chave compartilhada por várias
// runs do mesmo workflow — num worker pool, e só anexa o ponteiro quando
// há histórico (avg > 0). Mutação direta no slice recebido.
func enrichETA(ctx context.Context, opts Options, all []Run) {
	type wfKey struct{ repo, workflow string }
	seen := map[wfKey]struct{}{}
	var keys []wfKey
	for _, r := range all {
		k := wfKey{r.Repo, r.Workflow}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}

	avgs := make(map[wfKey]int64, len(keys))
	var avgsMu sync.Mutex
	runJobs(ctx, opts.Concurrency, len(keys), func(i int) {
		k := keys[i]
		avg, err := computeAvgDurationSec(ctx, opts, k.repo, k.workflow, opts.HistoryLimit)
		if err != nil {
			return
		}
		if avg == 0 {
			return
		}
		avgsMu.Lock()
		avgs[k] = avg
		avgsMu.Unlock()
	})

	for i := range all {
		if v, ok := avgs[wfKey{all[i].Repo, all[i].Workflow}]; ok {
			vCopy := v
			all[i].AvgDurationSec = &vCopy
		}
	}
}

// listRuns invoca `gh run list --repo X --status Y` e converte cada item
// pro shape canônico Run. Erros são propagados; output vazio devolve [].
func listRuns(ctx context.Context, opts Options, repo, status string, limit int) ([]Run, error) {
	out, err := opts.RunFn(ctx, "gh", "run", "list",
		"--repo", repo,
		"--status", status,
		"--limit", strconv.Itoa(limit),
		"--json", "databaseId,displayTitle,workflowName,event,headBranch,createdAt,url,status",
	)
	if err != nil {
		return nil, fmt.Errorf("gh run list %s %s: %w", repo, status, err)
	}
	type ghRun struct {
		DatabaseID   int64     `json:"databaseId"`
		DisplayTitle string    `json:"displayTitle"`
		WorkflowName string    `json:"workflowName"`
		Event        string    `json:"event"`
		HeadBranch   string    `json:"headBranch"`
		CreatedAt    time.Time `json:"createdAt"`
		URL          string    `json:"url"`
		Status       string    `json:"status"`
	}
	var raw []ghRun
	if len(out) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh run list %s %s: %w", repo, status, err)
	}
	runs := make([]Run, 0, len(raw))
	for _, g := range raw {
		runs = append(runs, Run{
			DatabaseID: g.DatabaseID,
			Repo:       repo,
			Workflow:   g.WorkflowName,
			Title:      g.DisplayTitle,
			Event:      g.Event,
			Branch:     g.HeadBranch,
			Status:     g.Status,
			URL:        g.URL,
			CreatedAt:  g.CreatedAt,
		})
	}
	return runs, nil
}

// computeAvgDurationSec retorna a média de duração (em segundos) das
// últimas N runs success do workflow indicado. Retorna 0 quando não há
// histórico suficiente — caller trata como "desconhecido".
func computeAvgDurationSec(ctx context.Context, opts Options, repo, workflow string, limit int) (int64, error) {
	out, err := opts.RunFn(ctx, "gh", "run", "list",
		"--repo", repo,
		"--status", "success",
		"--limit", strconv.Itoa(limit),
		"--json", "workflowName,createdAt,updatedAt",
	)
	if err != nil {
		return 0, err
	}
	type histEntry struct {
		WorkflowName string    `json:"workflowName"`
		CreatedAt    time.Time `json:"createdAt"`
		UpdatedAt    time.Time `json:"updatedAt"`
	}
	var raw []histEntry
	if len(out) == 0 {
		return 0, nil
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return 0, err
	}
	var sum, count int64
	for _, h := range raw {
		if h.WorkflowName != workflow {
			continue
		}
		d := h.UpdatedAt.Sub(h.CreatedAt)
		if d <= 0 {
			continue
		}
		sum += int64(d.Seconds())
		count++
	}
	if count == 0 {
		return 0, nil
	}
	return sum / count, nil
}

// summarize calcula contadores + ETA total. Para in_progress, ETA do run
// = max(0, avg - elapsed). Para queued, ETA = avg cheio. Runs sem avg
// não entram na soma.
func summarize(runs []Run, now time.Time) Summary {
	var s Summary
	for _, r := range runs {
		switch r.Status {
		case "in_progress":
			s.InProgress++
			if r.AvgDurationSec != nil {
				elapsed := int64(now.Sub(r.CreatedAt).Seconds())
				remaining := *r.AvgDurationSec - elapsed
				if remaining > 0 {
					s.ETATotalSec += remaining
				}
			}
		case "queued":
			s.Queued++
			if r.AvgDurationSec != nil {
				s.ETATotalSec += *r.AvgDurationSec
			}
		}
	}
	return s
}

// inferReposFromSystemd extrai repos únicos owner/repo dos units; ignora
// repos sem barra (org-level) porque gh run list exige owner/repo.
func inferReposFromSystemd(systemd []runner.Status) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range systemd {
		repo := s.Repo
		if repo == "" || !validRepoShape.MatchString(repo) {
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

var validRepoShape = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var validOrgShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

func inferOrgsFromSystemd(systemd []runner.Status) []string {
	seen := map[string]struct{}{}
	var orgs []string
	for _, status := range systemd {
		org := status.Repo
		if !validOrgShape.MatchString(org) {
			continue
		}
		if _, ok := seen[org]; ok {
			continue
		}
		seen[org] = struct{}{}
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)
	return orgs
}

func defaultReposConfig() ([]string, error) {
	data, err := os.ReadFile("/etc/civm/run-reaper.env")
	if os.IsNotExist(err) || os.IsPermission(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseReposConfig(data)
}

func parseReposConfig(data []byte) ([]string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key != "CIVM_REAPER_REPOS" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("parse CIVM_REAPER_REPOS: %w", err)
			}
			value = unquoted
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}
		var repos []string
		for _, repo := range strings.Split(value, ",") {
			if repo = strings.TrimSpace(repo); repo != "" {
				repos = append(repos, repo)
			}
		}
		return repos, nil
	}
	return nil, nil
}

func orgReposFromGitHub(ctx context.Context, runFn func(context.Context, string, ...string) ([]byte, error), org string) ([]string, error) {
	out, err := runFn(ctx, "gh", "api", "--paginate", "--slurp",
		"/orgs/"+org+"/repos?per_page=100")
	if err != nil {
		return nil, err
	}
	var pages [][]struct {
		FullName string `json:"full_name"`
		Archived bool   `json:"archived"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("parse paginated org repos: %w", err)
	}
	var repos []string
	for _, page := range pages {
		for _, repo := range page {
			if !repo.Archived {
				repos = append(repos, repo.FullName)
			}
		}
	}
	sort.Strings(repos)
	return repos, nil
}

func validateRepos(repos []string) error {
	for _, r := range repos {
		if err := civm.ValidateRepo(r); err != nil {
			return err
		}
	}
	return nil
}

// runJobs roda n jobs concorrentemente com worker pool de tamanho
// concurrency. Idiomático stdlib (sync.WaitGroup + semáforo via channel).
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
	if opts.Limit <= 0 {
		opts.Limit = 5
	}
	if opts.HistoryLimit <= 0 {
		opts.HistoryLimit = 10
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.ReposConfigFn == nil {
		opts.ReposConfigFn = defaultReposConfig
	}
	if opts.OrgReposFn == nil {
		opts.OrgReposFn = func(ctx context.Context, org string) ([]string, error) {
			return orgReposFromGitHub(ctx, opts.RunFn, org)
		}
	}
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Render escreve uma view humana do report.
func (r Report) Render(w io.Writer) {
	fmt.Fprintf(w, "civm active-runs | runs=%d (in_progress=%d queued=%d) | ETA total=%s\n",
		len(r.Runs), r.Summary.InProgress, r.Summary.Queued,
		formatSeconds(r.Summary.ETATotalSec))
	if len(r.Runs) == 0 {
		fmt.Fprintln(w, "(nenhuma run em andamento)")
		return
	}
	fmt.Fprintf(w, "\n%-12s %-22s %-18s %-8s %s\n",
		"STATUS", "REPO", "WORKFLOW", "AGE", "TITLE")
	now := time.Now()
	for _, run := range r.Runs {
		age := now.Sub(run.CreatedAt).Round(time.Minute)
		fmt.Fprintf(w, "%-12s %-22s %-18s %-8s %s\n",
			run.Status, run.Repo, truncate(run.Workflow, 18),
			age, truncate(run.Title, 60))
	}
}

// RenderJSON escreve o report como JSON indentado.
func (r Report) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}

func formatSeconds(sec int64) string {
	if sec <= 0 {
		return "0s"
	}
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	min := sec / 60
	if min < 60 {
		rem := sec % 60
		if rem > 0 {
			return fmt.Sprintf("%dmin %ds", min, rem)
		}
		return fmt.Sprintf("%dmin", min)
	}
	hr := min / 60
	remMin := min % 60
	if remMin > 0 {
		return fmt.Sprintf("%dh %dmin", hr, remMin)
	}
	return fmt.Sprintf("%dh", hr)
}
