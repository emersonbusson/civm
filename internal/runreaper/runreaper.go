// Package runreaper cancels GitHub Actions workflow runs that are still
// queued / in_progress when they no longer matter for an open PR on a shared
// self-hosted fleet.
//
// GitHub does not auto-cancel runs when a PR closes or when a newer push
// supersedes an older head SHA. Without reaping, those runs sit in the runner
// queue forever and starve live work.
//
// A run is a reap candidate when it is a pull_request / pull_request_target
// event and either:
//  1. its head branch has no open PR in that repo (PR closed/merged/deleted), or
//  2. its head branch still has an open PR, but the run's head_sha is not the
//     open PR's current head (superseded by a newer push).
//
// Safety rails:
//   - Only event=pull_request / pull_request_target runs are ever cancelled —
//     push (main CI), schedule (crons) and workflow_dispatch are never touched.
//   - Current-head runs of open PRs are always kept.
//   - Dry-run by default (Execute=false only reports candidates).
//   - Per-repo cancel cap (anti-runaway); the excess is logged, never silent.
//
// Stdlib-only; all GitHub access goes through injected funcs so the logic is
// hermetically testable.
package runreaper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
)

// ErrRunAlreadyCompleted marks a cancel attempt against a run that GitHub's
// own cancel/force-cancel endpoints already reject as completed, even though
// the list API that surfaced it as a reap candidate still reports
// status=queued/in_progress. This is a known GitHub-side staleness (a run
// cancelled/finished weeks ago can outlive its "queued" listing indefinitely
// — confirmed live against acme/app run 26423751663/26423751642, created
// 2026-05-25, still listed queued today though `gh run cancel` already
// answered "Cannot cancel a run that is completed" on 2026-06-17). The
// reaper's job here is already done — GitHub agrees the run isn't running —
// so this is not a reaper failure and must not be reported as one.
var ErrRunAlreadyCompleted = errors.New("run already completed per github (list api desync)")

// DefaultMaxCancelPerRepo bounds how many runs a single tick may cancel per
// repo. A real backlog of hundreds is handled across successive ticks; the cap
// is a blast-radius guard against a logic bug, not a throughput limit.
const DefaultMaxCancelPerRepo = 400

// Run is a queued or in_progress workflow run considered for reaping.
type Run struct {
	ID        int64     `json:"id"`
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	Event     string    `json:"event"`
	Status    string    `json:"status"`
	Workflow  string    `json:"workflow"`
	CreatedAt time.Time `json:"created_at"`
}

// Options configures Reap. The Fn fields are injected in tests; production
// defaults are wired by applyDefaults.
type Options struct {
	Repos            []string
	Execute          bool
	MaxCancelPerRepo int
	RunFn            func(ctx context.Context, name string, args ...string) ([]byte, error)
	// OpenHeadsFn returns headRefName → headRefOid for every open PR in the
	// repo. Presence of a key means the branch still has an open PR; the value
	// is the PR's current tip and is used to detect superseded pushes.
	OpenHeadsFn  func(ctx context.Context, repo string) (map[string]string, error)
	ActiveRunsFn func(ctx context.Context, repo string) ([]Run, error)
	CancelFn     func(ctx context.Context, repo string, runID int64) error
	NowFn        func() time.Time
}

// Event is one structured line in the report.
type Event struct {
	Event    string `json:"event"`
	Severity string `json:"severity"`
	Repo     string `json:"repo,omitempty"`
	RunID    int64  `json:"run_id,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Executed bool   `json:"executed"`
}

// Report is the result of a reap pass.
type Report struct {
	Executed   bool     `json:"executed"`
	Repos      []string `json:"repos"`
	Scanned    int      `json:"scanned"`
	Candidates int      `json:"candidates"`
	Cancelled  int      `json:"cancelled"`
	Events     []Event  `json:"events"`
	Exit       int      `json:"exit"`
}

// Reap scans the configured repos and cancels (or, in dry-run, reports) every
// queued/in_progress pull_request run that is either on a closed branch or
// supersedido by a newer open-PR head SHA.
func Reap(ctx context.Context, opts Options) Report {
	applyDefaults(&opts)
	report := Report{Executed: opts.Execute}
	if len(opts.Repos) == 0 {
		report.add(Event{Event: "reap-skipped", Severity: "warning", Reason: "no-repos"})
		report.Exit = 1
		return report
	}
	for _, repo := range opts.Repos {
		if err := civm.ValidateRepo(repo); err != nil {
			report.add(Event{Event: "reap-skipped", Severity: "warning", Repo: repo, Reason: "repo-invalid", Detail: err.Error()})
			report.Exit = maxInt(report.Exit, 1)
			continue
		}
		report.Repos = append(report.Repos, repo)
		reapRepo(ctx, opts, repo, &report)
	}
	return report
}

func reapRepo(ctx context.Context, opts Options, repo string, report *Report) {
	openHeads, err := opts.OpenHeadsFn(ctx, repo)
	if err != nil {
		report.add(Event{Event: "reap-skipped", Severity: "warning", Repo: repo, Reason: "open-prs-failed", Detail: err.Error()})
		report.Exit = maxInt(report.Exit, 1)
		return
	}
	runs, err := opts.ActiveRunsFn(ctx, repo)
	if err != nil {
		report.add(Event{Event: "reap-skipped", Severity: "warning", Repo: repo, Reason: "active-runs-failed", Detail: err.Error()})
		report.Exit = maxInt(report.Exit, 1)
		return
	}
	report.Scanned += len(runs)

	type candidate struct {
		run    Run
		reason string
	}
	candidates := make([]candidate, 0, len(runs))
	for _, run := range runs {
		if !isPullRequestEvent(run.Event) {
			continue // never touch push / schedule / workflow_dispatch
		}
		if run.Branch == "" {
			continue // unknown branch → keep (fail-safe)
		}
		currentHead, open := openHeads[run.Branch]
		if !open {
			candidates = append(candidates, candidate{run: run, reason: "pr-not-open"})
			continue
		}
		// Superseded: open PR still exists, but this run was built on an older tip.
		// Empty HeadSHA on either side → keep (fail-safe: cannot prove supersession).
		if run.HeadSHA != "" && currentHead != "" && !shaEqual(run.HeadSHA, currentHead) {
			candidates = append(candidates, candidate{run: run, reason: "superseded-sha"})
		}
	}
	// Oldest first: those are the ones GitHub hands the runner next, so
	// cancelling them first frees the queue head soonest.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].run.CreatedAt.Before(candidates[j].run.CreatedAt)
	})

	for i, c := range candidates {
		report.Candidates++
		ev := Event{
			Event:    "run-reaped",
			Severity: "info",
			Repo:     repo,
			RunID:    c.run.ID,
			Branch:   c.run.Branch,
			Workflow: c.run.Workflow,
			Reason:   c.reason,
			Executed: opts.Execute,
		}
		if c.reason == "superseded-sha" {
			ev.Detail = fmt.Sprintf("run=%s open=%s", shortSHA(c.run.HeadSHA), shortSHA(openHeads[c.run.Branch]))
		}
		if i >= opts.MaxCancelPerRepo {
			report.add(Event{Event: "reap-capped", Severity: "warning", Repo: repo, Reason: "max-cancel-reached", Detail: fmt.Sprintf("%d candidates, cap %d — remainder deferred to next tick", len(candidates), opts.MaxCancelPerRepo)})
			break
		}
		if !opts.Execute {
			report.add(ev)
			continue
		}
		if err := opts.CancelFn(ctx, repo, c.run.ID); err != nil {
			if errors.Is(err, ErrRunAlreadyCompleted) {
				// GitHub already agrees the run is over -- there is nothing
				// left for the reaper to do. Reporting this identically to a
				// real cancel failure was the bug itself (validation.md
				// 2026-06-17): the silent 5-min retry against the same ghost
				// runs never looked different from a genuine problem, so a
				// real cancel failure could hide behind it indefinitely.
				ev.Severity = "info"
				ev.Reason = "already-completed-ghost"
				ev.Detail = err.Error()
				report.add(ev)
				continue
			}
			ev.Severity = "warning"
			ev.Reason = "cancel-failed"
			ev.Detail = err.Error()
			report.add(ev)
			report.Exit = maxInt(report.Exit, 1)
			continue
		}
		report.Cancelled++
		report.add(ev)
	}
}

// isPullRequestEvent gates reaping to PR-triggered runs only.
func isPullRequestEvent(event string) bool {
	switch event {
	case "pull_request", "pull_request_target":
		return true
	default:
		return false
	}
}

func shaEqual(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func applyDefaults(opts *Options) {
	if opts.MaxCancelPerRepo <= 0 {
		opts.MaxCancelPerRepo = DefaultMaxCancelPerRepo
	}
	if opts.NowFn == nil {
		opts.NowFn = time.Now
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.OpenHeadsFn == nil {
		opts.OpenHeadsFn = func(ctx context.Context, repo string) (map[string]string, error) {
			return listOpenHeads(ctx, repo, opts.RunFn)
		}
	}
	if opts.ActiveRunsFn == nil {
		opts.ActiveRunsFn = func(ctx context.Context, repo string) ([]Run, error) {
			return listActiveRuns(ctx, repo, opts.RunFn)
		}
	}
	if opts.CancelFn == nil {
		opts.CancelFn = func(ctx context.Context, repo string, runID int64) error {
			return cancelRun(ctx, repo, runID, opts.RunFn)
		}
	}
}

// defaultRun shells out and folds stderr into the returned error. Plain
// exec.ExitError.Error() is just "exit status N" -- Output() only captures
// stderr into ExitError.Stderr, it never surfaces in the error text -- so
// without this, cancelRun's already-completed detection would have nothing
// to match against and every gh api rejection would look identical.
func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return out, err
}

func listOpenHeads(ctx context.Context, repo string, runFn func(context.Context, string, ...string) ([]byte, error)) (map[string]string, error) {
	out, err := runFn(ctx, "gh", "pr", "list", "--repo", repo, "--state", "open", "--limit", "1000", "--json", "headRefName,headRefOid")
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []struct {
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh pr list: %w", err)
	}
	open := make(map[string]string, len(prs))
	for _, pr := range prs {
		if pr.HeadRefName != "" {
			// First open PR wins if two PRs share a branch (rare); last write
			// would flip supersession. Prefer the first listed (most recent
			// by gh default sort) is fine for fail-safe keep-current.
			if _, exists := open[pr.HeadRefName]; !exists {
				open[pr.HeadRefName] = pr.HeadRefOid
			}
		}
	}
	return open, nil
}

func listActiveRuns(ctx context.Context, repo string, runFn func(context.Context, string, ...string) ([]byte, error)) ([]Run, error) {
	var all []Run
	for _, status := range []string{"queued", "in_progress"} {
		endpoint := fmt.Sprintf("/repos/%s/actions/runs?status=%s&per_page=100", repo, status)
		out, err := runFn(ctx, "gh", "api", "--paginate", endpoint)
		if err != nil {
			return nil, fmt.Errorf("gh api actions/runs?status=%s: %w", status, err)
		}
		runs, err := parseActiveRuns(out, repo)
		if err != nil {
			return nil, err
		}
		all = append(all, runs...)
	}
	return all, nil
}

// parseActiveRuns decodes one or more concatenated `actions/runs` JSON objects
// (gh --paginate emits one object per page) into Run values.
func parseActiveRuns(out []byte, repo string) ([]Run, error) {
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var runs []Run
	for {
		var page struct {
			WorkflowRuns []struct {
				ID         int64     `json:"id"`
				HeadBranch string    `json:"head_branch"`
				HeadSHA    string    `json:"head_sha"`
				Event      string    `json:"event"`
				Status     string    `json:"status"`
				Name       string    `json:"name"`
				CreatedAt  time.Time `json:"created_at"`
			} `json:"workflow_runs"`
		}
		if err := dec.Decode(&page); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parse gh runs: %w", err)
		}
		for _, r := range page.WorkflowRuns {
			runs = append(runs, Run{
				ID:        r.ID,
				Repo:      repo,
				Branch:    r.HeadBranch,
				HeadSHA:   r.HeadSHA,
				Event:     r.Event,
				Status:    r.Status,
				Workflow:  r.Name,
				CreatedAt: r.CreatedAt,
			})
		}
	}
	return runs, nil
}

// cancelRun stops a workflow run. It tries force-cancel FIRST: a plain
// POST .../cancel is accepted (202) but NOT applied to a run stuck queued
// waiting for a busy self-hosted runner — GitHub only delivers that
// cancellation when the runner finally picks the job, so closed-PR runs linger
// in the queue indefinitely. POST .../force-cancel dislodges them immediately.
// GitHub may reject force-cancel on a run that has not yet been regular-cancelled
// (or too soon after); in that case fall back to the plain cancel so the next
// reaper tick can force-cancel it. Returns nil if either call is accepted.
func cancelRun(ctx context.Context, repo string, runID int64, runFn func(context.Context, string, ...string) ([]byte, error)) error {
	id := strconv.FormatInt(runID, 10)
	force := fmt.Sprintf("/repos/%s/actions/runs/%s/force-cancel", repo, id)
	_, forceErr := runFn(ctx, "gh", "api", "-X", "POST", force, "--silent")
	if forceErr == nil {
		return nil
	}
	normal := fmt.Sprintf("/repos/%s/actions/runs/%s/cancel", repo, id)
	_, normalErr := runFn(ctx, "gh", "api", "-X", "POST", normal, "--silent")
	if normalErr == nil {
		return nil
	}
	if isAlreadyCompletedError(forceErr) || isAlreadyCompletedError(normalErr) {
		return fmt.Errorf("%w (force-cancel: %s; cancel: %s)", ErrRunAlreadyCompleted, forceErr.Error(), normalErr.Error())
	}
	return fmt.Errorf("gh api cancel/force-cancel run %s: %w", id, normalErr)
}

// isAlreadyCompletedError matches GitHub's rejection text when a run's own
// cancel/force-cancel endpoints already consider it finished (or otherwise
// non-cancellable because it is not a live work item), regardless of what the
// list API that surfaced it as a candidate still reports.
//
// Known live strings (validation.md 2026-06-17 + 2026-07-09):
//   - "Cannot cancel a run that is completed."
//   - "Cannot cancel a workflow run that is completed." (HTTP 409; run
//     completed between the active-list scan and the cancel request)
//   - "Cannot cancel a workflow re-run that has not yet queued." (HTTP 409;
//     May-2026 ghosts still listed as status=queued for months)
func isAlreadyCompletedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "cannot cancel a run that is completed") ||
		strings.Contains(msg, "cannot cancel a workflow run that is completed") ||
		strings.Contains(msg, "cannot cancel a workflow re-run that has not yet queued")
}

func maxInt(a, b int) int {
	if b > a {
		return b
	}
	return a
}

func (r *Report) add(ev Event) {
	if ev.Severity == "" {
		ev.Severity = "info"
	}
	r.Events = append(r.Events, ev)
}

// RenderJSON writes the report as indented JSON.
func (r Report) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Render writes a human-readable summary.
func (r Report) Render(w io.Writer) {
	mode := "DRY-RUN"
	if r.Executed {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "civmctl reap-runs: %s | exit=%d | scanned=%d candidates=%d cancelled=%d\n", mode, r.Exit, r.Scanned, r.Candidates, r.Cancelled)
	if len(r.Repos) > 0 {
		fmt.Fprintf(w, "Repos: %s\n", strings.Join(r.Repos, ","))
	}
	for _, ev := range r.Events {
		target := ev.Repo
		if ev.RunID != 0 {
			target = fmt.Sprintf("%s run=%d", target, ev.RunID)
		}
		detail := ev.Reason
		if ev.Detail != "" {
			detail += ": " + ev.Detail
		}
		if ev.Branch != "" {
			detail = fmt.Sprintf("%s [%s]", detail, ev.Branch)
		}
		fmt.Fprintf(w, "  %-14s %-8s %-40s %s\n", ev.Event, ev.Severity, target, detail)
	}
}
