package activeruns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/runner"
)

// Fixed clock used across tests for deterministic ETA math.
var testNow = time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testNow }

// scriptedRunFn returns a RunFn whose response is decided by matching the
// argv against the keyed table. Allows asserting argv shape and returning
// stable JSON without spinning up real shells.
func scriptedRunFn(table map[string]string, errs map[string]error) func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		for k, v := range table {
			if strings.Contains(key, k) {
				if err, ok := errs[k]; ok {
					return []byte(v), err
				}
				return []byte(v), nil
			}
		}
		return []byte("[]"), nil
	}
}

func TestCollectListsRunsAndComputesETA(t *testing.T) {
	// 2 repos, in_progress + queued per repo, + history per (repo,workflow).
	table := map[string]string{
		"gh run list --repo a/b --status in_progress": `[
			{"databaseId": 1, "displayTitle":"feat:x", "workflowName":"CI", "event":"push", "headBranch":"main",
			 "createdAt":"2026-05-23T11:55:00Z", "url":"https://github.com/a/b/actions/runs/1", "status":"in_progress"}
		]`,
		"gh run list --repo a/b --status queued": `[
			{"databaseId": 2, "displayTitle":"feat:y", "workflowName":"CI", "event":"pull_request", "headBranch":"feat",
			 "createdAt":"2026-05-23T11:58:00Z", "url":"https://github.com/a/b/actions/runs/2", "status":"queued"}
		]`,
		"gh run list --repo c/d --status in_progress": `[]`,
		"gh run list --repo c/d --status queued":      `[]`,
		// History for ETA: avg of 60s and 120s = 90s
		"gh run list --repo a/b --status success": `[
			{"workflowName":"CI","createdAt":"2026-05-20T00:00:00Z","updatedAt":"2026-05-20T00:01:00Z"},
			{"workflowName":"CI","createdAt":"2026-05-21T00:00:00Z","updatedAt":"2026-05-21T00:02:00Z"},
			{"workflowName":"Other","createdAt":"2026-05-22T00:00:00Z","updatedAt":"2026-05-22T01:00:00Z"}
		]`,
	}
	opts := DefaultOptions()
	opts.Repos = []string{"a/b", "c/d"}
	opts.Now = fixedNow
	opts.RunFn = scriptedRunFn(table, nil)

	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if r.Exit != 0 {
		t.Fatalf("Exit = %d, want 0; runs=%+v", r.Exit, r.Runs)
	}
	if got, want := r.Summary.InProgress, 1; got != want {
		t.Errorf("Summary.InProgress = %d, want %d", got, want)
	}
	if got, want := r.Summary.Queued, 1; got != want {
		t.Errorf("Summary.Queued = %d, want %d", got, want)
	}
	// in_progress: started 5min ago, avg 90s → remaining = max(0, 90 - 300) = 0
	// queued: avg 90s
	// total ETA: 90
	if got, want := r.Summary.ETATotalSec, int64(90); got != want {
		t.Errorf("Summary.ETATotalSec = %d, want %d", got, want)
	}
	if len(r.Runs) != 2 {
		t.Fatalf("Runs len = %d, want 2; got %+v", len(r.Runs), r.Runs)
	}
	// Sort: in_progress first
	if r.Runs[0].Status != "in_progress" || r.Runs[1].Status != "queued" {
		t.Errorf("sort order wrong: %s, %s", r.Runs[0].Status, r.Runs[1].Status)
	}
	for _, run := range r.Runs {
		if run.AvgDurationSec == nil {
			t.Errorf("run %d missing AvgDurationSec", run.DatabaseID)
			continue
		}
		if *run.AvgDurationSec != 90 {
			t.Errorf("run %d avg = %d, want 90", run.DatabaseID, *run.AvgDurationSec)
		}
	}
}

func TestCollectInferReposFromSystemd(t *testing.T) {
	opts := DefaultOptions()
	opts.InferRepos = true
	opts.Now = fixedNow
	opts.RunFn = scriptedRunFn(map[string]string{
		"gh run list --repo owner/repo1 --status in_progress": `[]`,
		"gh run list --repo owner/repo1 --status queued":      `[]`,
	}, nil)
	opts.SystemRunnersFn = func(_ context.Context) ([]runner.Status, error) {
		return []runner.Status{
			{UnitName: "actions.runner.owner-repo1.civm-r1.service", Repo: "owner/repo1", Name: "civm-r1"},
			{UnitName: "actions.runner.owner-repo1.civm-r2.service", Repo: "owner/repo1", Name: "civm-r2"},
			// org-level (no slash) — should be skipped:
			{UnitName: "actions.runner.org.civm-org.service", Repo: "org", Name: "civm-org"},
		}, nil
	}

	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if r.Summary.InProgress != 0 || r.Summary.Queued != 0 {
		t.Errorf("expected zero runs in summary, got %+v", r.Summary)
	}
}

func TestCollectInferReposFromReaperConfigForOrgRunner(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.InferRepos = true
	opts.IncludeETA = false
	opts.SystemRunnersFn = func(context.Context) ([]runner.Status, error) {
		return []runner.Status{{Repo: "acme", Name: "civm-acme-org"}}, nil
	}
	opts.ReposConfigFn = func() ([]string, error) {
		return []string{"acme/app"}, nil
	}
	var calls atomic.Int32
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "gh" {
			t.Fatalf("command = %q, want gh", name)
		}
		calls.Add(1)
		if !slices.Contains(args, "acme/app") {
			t.Fatalf("args = %v, want configured repo", args)
		}
		return []byte("[]"), nil
	}

	report, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, want 0", report.Exit)
	}
	if calls.Load() != 2 {
		t.Fatalf("gh calls = %d, want 2 statuses", calls.Load())
	}
}

func TestCollectInferReposFromOrgRunnerWhenConfigIsRestricted(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.InferRepos = true
	opts.IncludeETA = false
	opts.SystemRunnersFn = func(context.Context) ([]runner.Status, error) {
		return []runner.Status{{Repo: "acme", Name: "civm-acme-org"}}, nil
	}
	opts.ReposConfigFn = func() ([]string, error) { return nil, nil }
	opts.OrgReposFn = func(_ context.Context, org string) ([]string, error) {
		if org != "acme" {
			t.Fatalf("org = %q, want acme", org)
		}
		return []string{"acme/app"}, nil
	}
	var calls atomic.Int32
	opts.RunFn = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls.Add(1)
		if !slices.Contains(args, "acme/app") {
			t.Fatalf("args = %v, want expanded org repo", args)
		}
		return []byte("[]"), nil
	}

	report, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if report.Exit != 0 || calls.Load() != 2 {
		t.Fatalf("report=%+v calls=%d, want clean two-status collection", report, calls.Load())
	}
}

func TestParseReposConfigAcceptsSystemdQuotes(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"CIVM_REAPER_REPOS=acme/app,acme/docs\n",
		"CIVM_REAPER_REPOS=\"acme/app,acme/docs\"\n",
		"CIVM_REAPER_REPOS='acme/app,acme/docs'\n",
	} {
		got, err := parseReposConfig([]byte(raw))
		if err != nil || !slices.Equal(got, []string{"acme/app", "acme/docs"}) {
			t.Fatalf("parseReposConfig(%q) = %v, %v", raw, got, err)
		}
	}
}

func TestOrgReposFromGitHubPaginatesAndSkipsArchived(t *testing.T) {
	t.Parallel()
	runFn := func(_ context.Context, name string, args ...string) ([]byte, error) {
		want := []string{"api", "--paginate", "--slurp", "/orgs/acme/repos?per_page=100"}
		if name != "gh" || !slices.Equal(args, want) {
			t.Fatalf("command = %s %v, want gh %v", name, args, want)
		}
		return []byte(`[[{"full_name":"acme/a","archived":false}],` +
			`[{"full_name":"acme/old","archived":true},{"full_name":"acme/b","archived":false}]]`), nil
	}
	got, err := orgReposFromGitHub(context.Background(), runFn, "acme")
	if err != nil || !slices.Equal(got, []string{"acme/a", "acme/b"}) {
		t.Fatalf("orgReposFromGitHub = %v, %v", got, err)
	}
}

func TestCollectPartialErrorMarksExitNonZero(t *testing.T) {
	opts := DefaultOptions()
	opts.Repos = []string{"a/b"}
	opts.Now = fixedNow
	opts.RunFn = scriptedRunFn(
		map[string]string{
			"gh run list --repo a/b --status in_progress": `[
				{"databaseId": 5, "displayTitle":"x", "workflowName":"CI", "event":"push",
				 "headBranch":"m", "createdAt":"2026-05-23T11:59:00Z",
				 "url":"https://github.com/a/b/actions/runs/5", "status":"in_progress"}
			]`,
			"gh run list --repo a/b --status queued":  `[]`,
			"gh run list --repo a/b --status success": `[]`,
		},
		map[string]error{
			"gh run list --repo a/b --status queued": errors.New("auth failed"),
		},
	)

	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if r.Exit == 0 {
		t.Errorf("Exit should be non-zero when at least one gh call errored; got 0")
	}
	// in_progress run still present despite queued failure
	if len(r.Runs) != 1 || r.Runs[0].DatabaseID != 5 {
		t.Errorf("expected the in_progress run preserved despite queued error; got %+v", r.Runs)
	}
}

func TestCollectMissingHistoryLeavesAvgNil(t *testing.T) {
	opts := DefaultOptions()
	opts.Repos = []string{"a/b"}
	opts.Now = fixedNow
	opts.RunFn = scriptedRunFn(map[string]string{
		"gh run list --repo a/b --status in_progress": `[
			{"databaseId":7,"displayTitle":"z","workflowName":"OnlyMe","event":"push",
			 "headBranch":"m","createdAt":"2026-05-23T11:30:00Z",
			 "url":"https://github.com/a/b/actions/runs/7","status":"in_progress"}
		]`,
		"gh run list --repo a/b --status queued":  `[]`,
		"gh run list --repo a/b --status success": `[]`,
	}, nil)

	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(r.Runs) != 1 || r.Runs[0].AvgDurationSec != nil {
		t.Errorf("AvgDurationSec should be nil without history; got %+v", r.Runs)
	}
	if r.Summary.ETATotalSec != 0 {
		t.Errorf("ETATotalSec should be 0 when no avgs known; got %d", r.Summary.ETATotalSec)
	}
}

func TestCollectIncludeETAFalseSkipsHistoryCalls(t *testing.T) {
	var successCalls atomic.Int64
	opts := DefaultOptions()
	opts.Repos = []string{"a/b"}
	opts.IncludeETA = false
	opts.Now = fixedNow
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if strings.Contains(key, "--status success") {
			successCalls.Add(1)
		}
		if strings.Contains(key, "--status in_progress") {
			return []byte(`[{"databaseId":1,"displayTitle":"t","workflowName":"W","event":"push",
				"headBranch":"m","createdAt":"2026-05-23T11:59:00Z",
				"url":"https://github.com/a/b/actions/runs/1","status":"in_progress"}]`), nil
		}
		return []byte("[]"), nil
	}

	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if successCalls.Load() != 0 {
		t.Errorf("history call made despite IncludeETA=false (%d calls)", successCalls.Load())
	}
	if len(r.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(r.Runs))
	}
	if r.Runs[0].AvgDurationSec != nil {
		t.Errorf("AvgDurationSec must be nil when IncludeETA=false")
	}
}

func TestCollectRunsConcurrently(t *testing.T) {
	// Verifica que com concurrency >= len(jobs) o tempo wall é dominado
	// por um único delay (não soma de todos).
	delay := 80 * time.Millisecond
	repos := []string{"a/b", "c/d", "e/f", "g/h"}
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	opts := DefaultOptions()
	opts.Repos = repos
	opts.IncludeETA = false
	opts.Now = fixedNow
	opts.Concurrency = 8
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(delay)
		inFlight.Add(-1)
		return []byte("[]"), nil
	}

	start := time.Now()
	_, err := Collect(context.Background(), opts)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	expected := delay * time.Duration(len(repos)*2)
	if elapsed >= expected/2 {
		t.Errorf("Collect took %v, expected well under %v if concurrent", elapsed, expected/2)
	}
	if maxInFlight.Load() < 2 {
		t.Errorf("max parallel calls = %d, expected ≥ 2", maxInFlight.Load())
	}
}

func TestCollectValidatesRepos(t *testing.T) {
	opts := DefaultOptions()
	opts.Repos = []string{"bad repo"}
	opts.Now = fixedNow
	_, err := Collect(context.Background(), opts)
	if err == nil {
		t.Fatal("expected validation error for bad repo shape")
	}
}

func TestCollectEmptyReposReturnsEmptyReport(t *testing.T) {
	opts := DefaultOptions()
	opts.Now = fixedNow
	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(r.Runs) != 0 || r.Summary.InProgress+r.Summary.Queued != 0 {
		t.Errorf("expected empty report, got %+v", r)
	}
}

func TestRenderJSONRoundTrip(t *testing.T) {
	avg := int64(120)
	r := Report{
		CollectedAt: testNow,
		Runs: []Run{
			{DatabaseID: 1, Repo: "a/b", Workflow: "CI", Status: "in_progress",
				CreatedAt: testNow.Add(-time.Minute), AvgDurationSec: &avg},
		},
		Summary: Summary{InProgress: 1, ETATotalSec: 60},
		Exit:    0,
	}
	var buf bytes.Buffer
	if err := r.RenderJSON(&buf); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var got Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Summary.ETATotalSec != 60 || len(got.Runs) != 1 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.Runs[0].AvgDurationSec == nil || *got.Runs[0].AvgDurationSec != 120 {
		t.Errorf("AvgDurationSec lost in roundtrip: %+v", got.Runs[0].AvgDurationSec)
	}
}

func TestRenderHumanShowsCounts(t *testing.T) {
	r := Report{
		CollectedAt: testNow,
		Runs: []Run{
			{Status: "in_progress", Repo: "a/b", Workflow: "CI", Title: "x", CreatedAt: testNow.Add(-time.Minute)},
		},
		Summary: Summary{InProgress: 1, ETATotalSec: 60},
	}
	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()
	for _, want := range []string{"runs=1", "in_progress=1", "queued=0", "CI", "a/b"} {
		if !strings.Contains(got, want) {
			t.Errorf("Render output missing %q\noutput=\n%s", want, got)
		}
	}
}

func TestSummarizeNoAvg(t *testing.T) {
	runs := []Run{
		{Status: "in_progress", CreatedAt: testNow.Add(-time.Minute)},
		{Status: "queued", CreatedAt: testNow},
	}
	s := summarize(runs, testNow)
	if s.InProgress != 1 || s.Queued != 1 || s.ETATotalSec != 0 {
		t.Errorf("summary = %+v", s)
	}
}

func TestSummarizeMixedAvg(t *testing.T) {
	avg1 := int64(60)  // queued: ETA = 60
	avg2 := int64(180) // in_progress 60s ago: ETA = 180-60 = 120
	avg3 := int64(30)  // in_progress 60s ago: ETA = max(0, 30-60) = 0
	runs := []Run{
		{Status: "queued", AvgDurationSec: &avg1, CreatedAt: testNow},
		{Status: "in_progress", AvgDurationSec: &avg2, CreatedAt: testNow.Add(-time.Minute)},
		{Status: "in_progress", AvgDurationSec: &avg3, CreatedAt: testNow.Add(-time.Minute)},
	}
	s := summarize(runs, testNow)
	if s.ETATotalSec != 180 {
		t.Errorf("ETATotalSec = %d, want 180", s.ETATotalSec)
	}
}

func TestInferReposFromSystemdDedupAndFilter(t *testing.T) {
	got := inferReposFromSystemd([]runner.Status{
		{Repo: "a/b"},
		{Repo: "a/b"}, // duplicate
		{Repo: ""},
		{Repo: "org"}, // no slash → skipped
		{Repo: "c/d"},
	})
	if len(got) != 2 {
		t.Fatalf("got %v, expected 2 unique repos", got)
	}
	if got[0] != "a/b" || got[1] != "c/d" {
		t.Errorf("got %v, expected sorted [a/b c/d]", got)
	}
}

func TestFormatSeconds(t *testing.T) {
	cases := map[int64]string{
		0:    "0s",
		-5:   "0s",
		30:   "30s",
		60:   "1min",
		90:   "1min 30s",
		3600: "1h",
		3720: "1h 2min",
	}
	for in, want := range cases {
		got := formatSeconds(in)
		if got != want {
			t.Errorf("formatSeconds(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"short", "short"},
		{"exactly20chars-here!", "exactly20chars-here!"},
		{"this is a long string that exceeds twenty", "this is a long stri…"},
	}
	for _, c := range cases {
		got := truncate(c.in, 20)
		if got != c.want {
			t.Errorf("truncate(%q, 20) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRunJobsRespectsConcurrencyCap(t *testing.T) {
	var current atomic.Int32
	var maxSeen atomic.Int32
	var wg sync.WaitGroup
	wg.Add(20)
	fn := func(i int) {
		defer wg.Done()
		c := current.Add(1)
		for {
			old := maxSeen.Load()
			if c <= old || maxSeen.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
	}
	runJobs(context.Background(), 3, 20, fn)
	wg.Wait()
	if maxSeen.Load() > 3 {
		t.Errorf("maxSeen = %d, want ≤ 3", maxSeen.Load())
	}
}

func TestRunJobsZeroNoop(t *testing.T) {
	called := false
	runJobs(context.Background(), 4, 0, func(int) { called = true })
	if called {
		t.Error("fn called with n=0")
	}
}

// ExampleReport demonstrates JSON shape for downstream consumers.
func ExampleReport_RenderJSON() {
	avg := int64(120)
	r := Report{
		CollectedAt: time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		Runs: []Run{
			{DatabaseID: 1, Repo: "a/b", Workflow: "CI", Status: "queued",
				CreatedAt: time.Date(2026, 5, 23, 11, 58, 0, 0, time.UTC),
				URL:       "https://github.com/a/b/actions/runs/1", AvgDurationSec: &avg},
		},
		Summary: Summary{Queued: 1, ETATotalSec: 120},
	}
	_ = r.RenderJSON(&bytes.Buffer{})
	fmt.Println("ok")
	// Output: ok
}
