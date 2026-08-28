package actionsmetrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/runner"
)

var testNow = time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testNow }

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
		return []byte(`{}`), nil
	}
}

func TestCollectAggregatesBillingAndRuns(t *testing.T) {
	billing := `{
		"usageItems": [
			{"date":"2026-05-01T00:00:00Z","product":"actions","sku":"Actions Linux","quantity":3156,"unitType":"Minutes","organizationName":"acme","repositoryName":"app"},
			{"date":"2026-05-01T00:00:00Z","product":"actions","sku":"Actions Linux","quantity":2019,"unitType":"Minutes","organizationName":"acme","repositoryName":"sample-repo"},
			{"date":"2026-05-01T00:00:00Z","product":"actions","sku":"Actions storage","quantity":0.5,"unitType":"GigabyteHours","organizationName":"acme","repositoryName":"civm"},
			{"date":"2026-05-01T00:00:00Z","product":"copilot","sku":"Copilot Business","quantity":1,"unitType":"User","organizationName":"acme","repositoryName":""}
		]
	}`
	runsAcme := `{"total_count": 4436}`
	runsCivm := `{"total_count": 121}`

	opts := DefaultOptions()
	opts.Organization = "acme"
	opts.Repos = []string{"acme/app", "acme/civm"}
	opts.StartDate = "2026-05-01"
	opts.EndDate = "2026-05-23"
	opts.Now = fixedNow
	opts.RunFn = scriptedRunFn(map[string]string{
		"organizations/acme/settings/billing/usage": billing,
		"repos/acme/app/actions/runs":             runsAcme,
		"repos/acme/civm/actions/runs":              runsCivm,
	}, nil)

	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if r.Exit != 0 {
		t.Errorf("Exit = %d, want 0", r.Exit)
	}
	if r.TotalMinutes != 3156+2019 {
		t.Errorf("TotalMinutes = %d, want %d", r.TotalMinutes, 3156+2019)
	}
	if r.TotalRuns != 4436+121 {
		t.Errorf("TotalRuns = %d, want %d", r.TotalRuns, 4436+121)
	}
	// SKU filtra Copilot (não-actions)
	if len(r.BySKU) != 2 {
		t.Fatalf("BySKU len = %d, want 2 (Linux Minutes + storage GB-h); got %+v", len(r.BySKU), r.BySKU)
	}
	if r.BySKU[0].SKU != "Actions Linux" {
		t.Errorf("BySKU[0] = %q, want Actions Linux", r.BySKU[0].SKU)
	}
	if r.BySKU[0].Minutes != 5175 {
		t.Errorf("Actions Linux total = %d, want 5175", r.BySKU[0].Minutes)
	}
	// ByRepo ordenado por minutes desc
	if len(r.ByRepo) != 2 {
		t.Fatalf("ByRepo len = %d, want 2", len(r.ByRepo))
	}
	if r.ByRepo[0].Repo != "acme/app" || r.ByRepo[0].HostedMinutes != 3156 {
		t.Errorf("ByRepo[0] = %+v, want acme/app 3156min", r.ByRepo[0])
	}
}

func TestCollectInferReposFromSystemd(t *testing.T) {
	opts := DefaultOptions()
	opts.Organization = "acme"
	opts.InferRepos = true
	opts.Now = fixedNow
	opts.RunFn = scriptedRunFn(map[string]string{
		"organizations/acme/settings/billing/usage": `{"usageItems": []}`,
		"repos/owner/repo1/actions/runs":             `{"total_count": 10}`,
	}, nil)
	opts.SystemRunnersFn = func(_ context.Context) ([]runner.Status, error) {
		return []runner.Status{
			{Repo: "owner/repo1"},
			{Repo: "owner/repo1"}, // dup
			{Repo: "org"},         // sem slash, skipa
		}, nil
	}
	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(r.ByRepo) != 1 || r.ByRepo[0].Repo != "owner/repo1" {
		t.Fatalf("ByRepo = %+v", r.ByRepo)
	}
	if r.ByRepo[0].RunsTotal != 10 {
		t.Errorf("RunsTotal = %d, want 10", r.ByRepo[0].RunsTotal)
	}
}

func TestCollectBillingErrorMarksExitNonZero(t *testing.T) {
	opts := DefaultOptions()
	opts.Organization = "acme"
	opts.Repos = []string{"acme/civm"}
	opts.Now = fixedNow
	opts.RunFn = scriptedRunFn(
		map[string]string{
			"organizations/acme/settings/billing/usage": `{"message":"forbidden"}`,
			"repos/acme/civm/actions/runs":              `{"total_count": 5}`,
		},
		map[string]error{
			"organizations/acme/settings/billing/usage": errors.New("forbidden"),
		},
	)
	r, err := Collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if r.Exit == 0 {
		t.Errorf("Exit should be non-zero when billing fails")
	}
	// run counts ainda devem completar
	if r.TotalRuns != 5 {
		t.Errorf("TotalRuns = %d, want 5 even with billing error", r.TotalRuns)
	}
}

func TestCollectInvalidDateRejected(t *testing.T) {
	opts := DefaultOptions()
	opts.Organization = "acme"
	opts.StartDate = "not-a-date"
	opts.EndDate = "2026-05-23"
	_, err := Collect(context.Background(), opts)
	if err == nil {
		t.Fatal("expected date validation error")
	}
}

func TestCollectInvalidOrgRejected(t *testing.T) {
	opts := DefaultOptions()
	opts.Organization = "bad org!"
	_, err := Collect(context.Background(), opts)
	if err == nil {
		t.Fatal("expected org validation error")
	}
}

func TestCollectInvalidRepoRejected(t *testing.T) {
	opts := DefaultOptions()
	opts.Organization = "acme"
	opts.Repos = []string{"bad repo"}
	_, err := Collect(context.Background(), opts)
	if err == nil {
		t.Fatal("expected repo validation error")
	}
}

func TestCollectRunsConcurrently(t *testing.T) {
	delay := 80 * time.Millisecond
	var maxInFlight, inFlight atomic.Int32
	opts := DefaultOptions()
	opts.Organization = "acme"
	opts.Repos = []string{"a/b", "c/d", "e/f", "g/h"}
	opts.Now = fixedNow
	opts.Concurrency = 4
	opts.RunFn = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		key := strings.Join(args, " ")
		// billing chamada — fora do paralelismo, conta separado:
		if strings.Contains(key, "billing/usage") {
			return []byte(`{"usageItems": []}`), nil
		}
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(delay)
		inFlight.Add(-1)
		return []byte(`{"total_count": 1}`), nil
	}

	start := time.Now()
	if _, err := Collect(context.Background(), opts); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	elapsed := time.Since(start)
	expectedSequential := delay * 4
	if elapsed >= expectedSequential*3/4 {
		t.Errorf("elapsed = %v, expected < %v if concurrent", elapsed, expectedSequential*3/4)
	}
	if maxInFlight.Load() < 2 {
		t.Errorf("maxInFlight = %d, expected ≥ 2", maxInFlight.Load())
	}
}

func TestPeriodHelper(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in         string
		wantStart  string
		wantEnd    string
		wantErr    bool
	}{
		{"month", "2026-05-01", "2026-05-23", false},
		{"", "2026-05-01", "2026-05-23", false},
		{"last-month", "2026-04-01", "2026-04-30", false},
		{"today", "2026-05-23", "2026-05-23", false},
		{"2026-01-01..2026-02-28", "2026-01-01", "2026-02-28", false},
		{"junk", "", "", true},
	}
	for _, c := range cases {
		s, e, err := Period(c.in, now)
		if c.wantErr && err == nil {
			t.Errorf("Period(%q): expected error", c.in)
			continue
		}
		if !c.wantErr && err != nil {
			t.Errorf("Period(%q): unexpected error %v", c.in, err)
			continue
		}
		if s != c.wantStart || e != c.wantEnd {
			t.Errorf("Period(%q) = (%q, %q), want (%q, %q)", c.in, s, e, c.wantStart, c.wantEnd)
		}
	}
}

func TestPeriodWeekStartsMonday(t *testing.T) {
	// Sat 2026-05-23, ISO week starts Monday → 2026-05-18
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	s, e, err := Period("week", now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if s != "2026-05-18" || e != "2026-05-23" {
		t.Errorf("week = %s..%s, want 2026-05-18..2026-05-23", s, e)
	}
}

func TestRenderJSONRoundTrip(t *testing.T) {
	r := Report{
		CollectedAt:  testNow,
		StartDate:    "2026-05-01",
		EndDate:      "2026-05-23",
		Organization: "acme",
		TotalMinutes: 5175,
		TotalRuns:    4557,
		BySKU:        []SKUTotal{{SKU: "Actions Linux", Minutes: 5175, UnitType: "Minutes"}},
		ByRepo:       []RepoMetric{{Repo: "acme/app", HostedMinutes: 3156, RunsTotal: 4436}},
	}
	var buf bytes.Buffer
	if err := r.RenderJSON(&buf); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var got Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.TotalMinutes != 5175 || got.TotalRuns != 4557 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestRenderHumanShowsTotals(t *testing.T) {
	r := Report{
		StartDate:    "2026-05-01",
		EndDate:      "2026-05-23",
		Organization: "acme",
		TotalMinutes: 5175,
		TotalRuns:    4557,
		BySKU:        []SKUTotal{{SKU: "Actions Linux", Minutes: 5175, UnitType: "Minutes"}},
		ByRepo:       []RepoMetric{{Repo: "acme/app", HostedMinutes: 3156, RunsTotal: 4436}},
	}
	var buf bytes.Buffer
	r.Render(&buf)
	out := buf.String()
	for _, want := range []string{"acme", "5175", "4557", "Actions Linux", "3156"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestValidOrgRejectsMaliciousInput(t *testing.T) {
	bad := []string{"", "a b", "../etc", "abc;rm -rf /", "$(whoami)", "ab|cd"}
	for _, s := range bad {
		if validOrg(s) {
			t.Errorf("validOrg(%q) returned true; should reject", s)
		}
	}
	good := []string{"acme", "acme-corp", "ABC", "a1", "x"}
	for _, s := range good {
		if !validOrg(s) {
			t.Errorf("validOrg(%q) returned false; should accept", s)
		}
	}
}
