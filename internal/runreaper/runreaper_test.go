package runreaper

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// mockGH dispatches a fake `gh` by inspecting the joined command line, so the
// default OpenHeadsFn/ActiveRunsFn/CancelFn (and their parsing) run for real.
func mockGH(openJSON, queuedJSON, inProgressJSON string, cancelErr error) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "pr list"):
			return []byte(openJSON), nil
		case strings.Contains(joined, "status=queued"):
			return []byte(queuedJSON), nil
		case strings.Contains(joined, "status=in_progress"):
			return []byte(inProgressJSON), nil
		case strings.Contains(joined, "cancel"):
			return nil, cancelErr
		}
		return nil, fmt.Errorf("unexpected gh call: %s", joined)
	}
}

func ts(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// fixture builds an Options wired with in-memory fakes and records cancels.
// open maps branch -> current open-PR head SHA (empty value still means open).
func fixture(open map[string]string, runs []Run, cancelled *[]int64) Options {
	return Options{
		Repos:   []string{"acme/app"},
		Execute: true,
		OpenHeadsFn: func(_ context.Context, _ string) (map[string]string, error) {
			return open, nil
		},
		ActiveRunsFn: func(_ context.Context, _ string) ([]Run, error) {
			return runs, nil
		},
		CancelFn: func(_ context.Context, _ string, id int64) error {
			*cancelled = append(*cancelled, id)
			return nil
		},
	}
}

func TestReapCancelsClosedPRRunsOnly(t *testing.T) {
	open := map[string]string{"feature/open-1": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	runs := []Run{
		{ID: 1, Event: "pull_request", Status: "queued", Branch: "feature/open-1"},       // keep: open PR
		{ID: 2, Event: "pull_request", Status: "queued", Branch: "fix/closed-pr"},        // reap
		{ID: 3, Event: "pull_request_target", Status: "in_progress", Branch: "old/gone"}, // reap
		{ID: 4, Event: "push", Status: "queued", Branch: "main"},                         // keep: not a PR event
		{ID: 5, Event: "schedule", Status: "queued", Branch: "nightly"},                  // keep: cron
		{ID: 6, Event: "pull_request", Status: "queued", Branch: ""},                     // keep: no branch
	}
	var cancelled []int64
	report := Reap(context.Background(), fixture(open, runs, &cancelled))

	if report.Exit != 0 {
		t.Fatalf("exit = %d, want 0", report.Exit)
	}
	if got, want := report.Cancelled, 2; got != want {
		t.Fatalf("cancelled = %d, want %d (events: %+v)", got, want, report.Events)
	}
	gotIDs := map[int64]bool{}
	for _, id := range cancelled {
		gotIDs[id] = true
	}
	if !gotIDs[2] || !gotIDs[3] {
		t.Errorf("expected runs 2 and 3 cancelled, got %v", cancelled)
	}
	for _, keep := range []int64{1, 4, 5, 6} {
		if gotIDs[keep] {
			t.Errorf("run %d should not have been cancelled", keep)
		}
	}
}

func TestReapDryRunCancelsNothing(t *testing.T) {
	runs := []Run{{ID: 7, Event: "pull_request", Status: "queued", Branch: "fix/closed"}}
	var cancelled []int64
	opts := fixture(nil, runs, &cancelled)
	opts.Execute = false
	report := Reap(context.Background(), opts)

	if len(cancelled) != 0 {
		t.Fatalf("dry-run cancelled %v, want none", cancelled)
	}
	if report.Candidates != 1 {
		t.Fatalf("candidates = %d, want 1", report.Candidates)
	}
	if report.Cancelled != 0 {
		t.Fatalf("cancelled count = %d, want 0", report.Cancelled)
	}
}

func TestReapOldestFirstAndCap(t *testing.T) {
	runs := []Run{
		{ID: 10, Event: "pull_request", Status: "queued", Branch: "c", CreatedAt: ts("2026-06-04T12:00:00Z")},
		{ID: 11, Event: "pull_request", Status: "queued", Branch: "c", CreatedAt: ts("2026-06-04T10:00:00Z")},
		{ID: 12, Event: "pull_request", Status: "queued", Branch: "c", CreatedAt: ts("2026-06-04T11:00:00Z")},
	}
	var cancelled []int64
	opts := fixture(nil, runs, &cancelled)
	opts.MaxCancelPerRepo = 2
	report := Reap(context.Background(), opts)

	if len(cancelled) != 2 {
		t.Fatalf("cancelled %d, want 2 (cap)", len(cancelled))
	}
	// Oldest two first: 11 (10:00) then 12 (11:00).
	if cancelled[0] != 11 || cancelled[1] != 12 {
		t.Errorf("cap order = %v, want [11 12] (oldest first)", cancelled)
	}
	if report.Cancelled != 2 {
		t.Errorf("report.Cancelled = %d, want 2", report.Cancelled)
	}
	var capped bool
	for _, ev := range report.Events {
		if ev.Event == "reap-capped" {
			capped = true
		}
	}
	if !capped {
		t.Error("expected a reap-capped event")
	}
}

func TestReapNoReposExits1(t *testing.T) {
	report := Reap(context.Background(), Options{})
	if report.Exit != 1 {
		t.Fatalf("exit = %d, want 1", report.Exit)
	}
}

func TestReapInvalidRepoSkipped(t *testing.T) {
	report := Reap(context.Background(), Options{Repos: []string{"not-a-repo"}})
	if report.Exit != 1 {
		t.Fatalf("exit = %d, want 1", report.Exit)
	}
	if report.Candidates != 0 {
		t.Fatalf("candidates = %d, want 0", report.Candidates)
	}
}

func TestReapCancelFailureSetsExit(t *testing.T) {
	runs := []Run{{ID: 9, Event: "pull_request", Status: "queued", Branch: "fix/closed"}}
	opts := Options{
		Repos:        []string{"acme/app"},
		Execute:      true,
		OpenHeadsFn:  func(_ context.Context, _ string) (map[string]string, error) { return nil, nil },
		ActiveRunsFn: func(_ context.Context, _ string) ([]Run, error) { return runs, nil },
		CancelFn:     func(_ context.Context, _ string, _ int64) error { return context.DeadlineExceeded },
	}
	report := Reap(context.Background(), opts)
	if report.Exit != 1 {
		t.Fatalf("exit = %d, want 1 on cancel failure", report.Exit)
	}
	if report.Cancelled != 0 {
		t.Fatalf("cancelled = %d, want 0", report.Cancelled)
	}
}

// TestDefaultRunFoldsStderrIntoError is the regression for the gap that made
// isAlreadyCompletedError unreachable in production: exec.ExitError.Error()
// alone is just "exit status N", the real gh api message lives in
// ExitError.Stderr and previously never reached the returned error text.
func TestDefaultRunFoldsStderrIntoError(t *testing.T) {
	_, err := defaultRun(context.Background(), "sh", "-c", "echo 'Cannot cancel a run that is completed.' >&2; exit 1")
	if err == nil {
		t.Fatal("expected a non-nil error from a failing command")
	}
	if !strings.Contains(err.Error(), "Cannot cancel a run that is completed") {
		t.Fatalf("error text lost stderr content: %v", err)
	}
}

func TestParseActiveRunsMultiPage(t *testing.T) {
	// Two concatenated pages as gh --paginate emits them.
	page := `{"workflow_runs":[{"id":1,"head_branch":"a","event":"pull_request","status":"queued","name":"Go CI","created_at":"2026-06-04T10:00:00Z"}]}` +
		`{"workflow_runs":[{"id":2,"head_branch":"b","event":"push","status":"in_progress","name":"Web CI","created_at":"2026-06-04T11:00:00Z"}]}`
	runs, err := parseActiveRuns([]byte(page), "acme/app")
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].ID != 1 || runs[0].Branch != "a" || runs[0].Workflow != "Go CI" {
		t.Errorf("run0 = %+v", runs[0])
	}
	if runs[1].ID != 2 || runs[1].Event != "push" {
		t.Errorf("run1 = %+v", runs[1])
	}
}

func TestRenderJSONRoundTrips(t *testing.T) {
	report := Report{Executed: true, Repos: []string{"acme/app"}, Cancelled: 1}
	var sb strings.Builder
	if err := report.RenderJSON(&sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(sb.String(), "\"cancelled\": 1") {
		t.Errorf("json missing cancelled field: %s", sb.String())
	}
}

// TestReapEndToEndViaRunFn drives Reap through the default gh-backed funcs
// (only RunFn injected), exercising applyDefaults, listOpenBranches,
// listActiveRuns, parseActiveRuns and cancelRun.
func TestReapEndToEndViaRunFn(t *testing.T) {
	open := `[{"headRefName":"feature/open-1","headRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`
	queued := `{"workflow_runs":[` +
		`{"id":1,"head_branch":"feature/open-1","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","event":"pull_request","status":"queued","name":"Go CI","created_at":"2026-06-04T10:00:00Z"},` +
		`{"id":2,"head_branch":"fix/closed","event":"pull_request","status":"queued","name":"Web CI","created_at":"2026-06-04T10:01:00Z"}]}`
	inprog := `{"workflow_runs":[{"id":3,"head_branch":"old/gone","event":"pull_request","status":"in_progress","name":"Docs CI","created_at":"2026-06-04T09:00:00Z"}]}`

	opts := Options{
		Repos:   []string{"acme/app"},
		Execute: true,
		RunFn:   mockGH(open, queued, inprog, nil),
	}
	report := Reap(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("exit = %d, want 0 (events %+v)", report.Exit, report.Events)
	}
	// runs 2 (fix/closed) and 3 (old/gone) reaped; run 1 (open) kept.
	if report.Cancelled != 2 {
		t.Fatalf("cancelled = %d, want 2", report.Cancelled)
	}
	if report.Scanned != 3 {
		t.Fatalf("scanned = %d, want 3", report.Scanned)
	}
}

func TestReapEndToEndCancelError(t *testing.T) {
	open := `[]`
	queued := `{"workflow_runs":[{"id":9,"head_branch":"fix/closed","event":"pull_request","status":"queued","name":"Go CI"}]}`
	inprog := `{"workflow_runs":[]}`
	opts := Options{
		Repos:   []string{"acme/app"},
		Execute: true,
		RunFn:   mockGH(open, queued, inprog, fmt.Errorf("boom")),
	}
	report := Reap(context.Background(), opts)
	if report.Exit != 1 {
		t.Fatalf("exit = %d, want 1 on cancel error", report.Exit)
	}
}

func TestReapOpenBranchesError(t *testing.T) {
	opts := Options{
		Repos: []string{"acme/app"},
		RunFn: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return nil, fmt.Errorf("gh down")
		},
	}
	report := Reap(context.Background(), opts)
	if report.Exit != 1 {
		t.Fatalf("exit = %d, want 1 when open-PR listing fails", report.Exit)
	}
}

func TestRenderHumanIncludesCounts(t *testing.T) {
	report := Reap(context.Background(), fixture(nil,
		[]Run{{ID: 5, Event: "pull_request", Status: "queued", Branch: "fix/x", Workflow: "Go CI"}},
		new([]int64)))
	var sb strings.Builder
	report.Render(&sb)
	out := sb.String()
	if !strings.Contains(out, "reap-runs") || !strings.Contains(out, "run-reaped") {
		t.Errorf("render output missing expected lines:\n%s", out)
	}
}

func TestCancelRunPrefersForceCancel(t *testing.T) {
	var calls []string
	runFn := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	if err := cancelRun(context.Background(), "acme/app", 42, runFn); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "force-cancel") {
		t.Fatalf("expected a single force-cancel call, got %v", calls)
	}
}

func TestCancelRunFallsBackToPlainCancel(t *testing.T) {
	var calls []string
	runFn := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		calls = append(calls, joined)
		if strings.Contains(joined, "force-cancel") {
			return nil, fmt.Errorf("422 force-cancel not yet allowed")
		}
		return nil, nil
	}
	if err := cancelRun(context.Background(), "acme/app", 42, runFn); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected force-cancel then plain cancel, got %v", calls)
	}
	if strings.Contains(calls[1], "force-cancel") || !strings.Contains(calls[1], "/cancel") {
		t.Errorf("second call should be the plain /cancel, got %q", calls[1])
	}
}

func TestCancelRunBothFail(t *testing.T) {
	runFn := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("network down")
	}
	if err := cancelRun(context.Background(), "acme/app", 42, runFn); err == nil {
		t.Fatal("expected error when both force-cancel and cancel fail")
	}
}

// TestCancelRunAlreadyCompletedWrapsSentinel reproduces the live GitHub
// rejection from validation.md (2026-06-17): both force-cancel and cancel
// reject a run the list API still shows queued, because GitHub's own state
// already considers it completed. cancelRun must surface this as
// ErrRunAlreadyCompleted so the caller can tell it apart from a genuine
// failure.
func TestCancelRunAlreadyCompletedWrapsSentinel(t *testing.T) {
	runFn := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "force-cancel") {
			return nil, fmt.Errorf("exit status 1: Cannot cancel a run that is completed.")
		}
		return nil, fmt.Errorf("exit status 1: Cannot cancel a run that is completed.")
	}
	err := cancelRun(context.Background(), "acme/app", 26423751663, runFn)
	if !errors.Is(err, ErrRunAlreadyCompleted) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRunAlreadyCompleted)", err)
	}
}

// TestCancelRunWorkflowAlreadyCompletedWrapsSentinel reproduces the live
// race observed on 2026-08-02: a superseded run completes between the active
// list scan and the cancel request, and GitHub includes "workflow" in the
// HTTP 409 message. The outcome is as benign as the older wording.
func TestCancelRunWorkflowAlreadyCompletedWrapsSentinel(t *testing.T) {
	runFn := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("gh: Cannot cancel a workflow run that is completed. (HTTP 409)")
	}
	err := cancelRun(context.Background(), "acme/app", 30745127312, runFn)
	if !errors.Is(err, ErrRunAlreadyCompleted) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRunAlreadyCompleted)", err)
	}
}

// TestCancelRunGenuineFailureDoesNotMatchSentinel guards against
// over-matching: a real, unrelated cancel failure (rate limit, auth, network)
// must never be misclassified as the benign ghost-run case.
func TestCancelRunGenuineFailureDoesNotMatchSentinel(t *testing.T) {
	runFn := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1: API rate limit exceeded")
	}
	err := cancelRun(context.Background(), "acme/app", 42, runFn)
	if errors.Is(err, ErrRunAlreadyCompleted) {
		t.Fatalf("a rate-limit error must not match ErrRunAlreadyCompleted, got %v", err)
	}
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
}

// TestReapAlreadyCompletedGhostIsInfoNotFailure is the end-to-end regression
// for the bug this fix closes: a candidate that GitHub already considers
// completed must not bump report.Exit, must not count as report.Cancelled
// (the reaper didn't cancel anything -- GitHub already had), and must be
// reported as its own distinct, non-warning event so a real cancel-failed
// never hides behind identical noise (see validation.md 2026-06-17).
func TestReapAlreadyCompletedGhostIsInfoNotFailure(t *testing.T) {
	runs := []Run{{ID: 26423751663, Event: "pull_request", Status: "queued", Branch: "feature/add-finance-module"}}
	opts := Options{
		Repos:        []string{"acme/app"},
		Execute:      true,
		OpenHeadsFn:  func(_ context.Context, _ string) (map[string]string, error) { return nil, nil },
		ActiveRunsFn: func(_ context.Context, _ string) ([]Run, error) { return runs, nil },
		CancelFn: func(_ context.Context, _ string, _ int64) error {
			return fmt.Errorf("%w: force-cancel and cancel both rejected", ErrRunAlreadyCompleted)
		},
	}
	report := Reap(context.Background(), opts)
	if report.Exit != 0 {
		t.Fatalf("exit = %d, want 0 -- an already-completed ghost is not a reaper failure", report.Exit)
	}
	if report.Cancelled != 0 {
		t.Fatalf("cancelled = %d, want 0 -- the reaper did not cancel anything, github already had", report.Cancelled)
	}
	var found bool
	for _, ev := range report.Events {
		if ev.Event == "run-reaped" && ev.Reason == "already-completed-ghost" {
			found = true
			if ev.Severity != "info" {
				t.Errorf("severity = %q, want %q", ev.Severity, "info")
			}
		}
		if ev.Reason == "cancel-failed" {
			t.Error("an already-completed ghost must never be reported under the generic cancel-failed reason")
		}
	}
	if !found {
		t.Fatalf("expected an already-completed-ghost event, got %+v", report.Events)
	}
}

func TestReapCancelsSupersededSHAOnOpenPR(t *testing.T) {
	// Branch still open at tip "bbbb..."; older run on "aaaa..." must reap.
	open := map[string]string{"feature/open-1": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	runs := []Run{
		{ID: 1, Event: "pull_request", Status: "queued", Branch: "feature/open-1", HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},      // current tip — keep
		{ID: 2, Event: "pull_request", Status: "queued", Branch: "feature/open-1", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},      // superseded
		{ID: 3, Event: "pull_request", Status: "in_progress", Branch: "feature/open-1", HeadSHA: "cccccccccccccccccccccccccccccccccccccccc"}, // superseded
		{ID: 4, Event: "push", Status: "queued", Branch: "feature/open-1", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},              // not a PR event — keep
		{ID: 5, Event: "pull_request", Status: "queued", Branch: "feature/open-1", HeadSHA: ""},                                              // empty sha — keep fail-safe
	}
	var cancelled []int64
	report := Reap(context.Background(), fixture(open, runs, &cancelled))
	if report.Exit != 0 {
		t.Fatalf("exit = %d, want 0 (events %+v)", report.Exit, report.Events)
	}
	if got, want := report.Cancelled, 2; got != want {
		t.Fatalf("cancelled = %d, want %d; got ids %v events %+v", got, want, cancelled, report.Events)
	}
	gotIDs := map[int64]bool{}
	for _, id := range cancelled {
		gotIDs[id] = true
	}
	if !gotIDs[2] || !gotIDs[3] {
		t.Errorf("expected runs 2 and 3 cancelled, got %v", cancelled)
	}
	for _, keep := range []int64{1, 4, 5} {
		if gotIDs[keep] {
			t.Errorf("run %d should not have been cancelled", keep)
		}
	}
	// reasons must be superseded-sha
	for _, ev := range report.Events {
		if ev.RunID == 2 || ev.RunID == 3 {
			if ev.Reason != "superseded-sha" {
				t.Errorf("run %d reason=%q want superseded-sha", ev.RunID, ev.Reason)
			}
		}
	}
}

func TestReapKeepsCurrentHeadCaseInsensitive(t *testing.T) {
	open := map[string]string{"feature/open-1": "ABCDEF0123456789ABCDEF0123456789ABCDEF01"}
	runs := []Run{
		{ID: 1, Event: "pull_request", Status: "queued", Branch: "feature/open-1", HeadSHA: "abcdef0123456789abcdef0123456789abcdef01"},
	}
	var cancelled []int64
	report := Reap(context.Background(), fixture(open, runs, &cancelled))
	if report.Cancelled != 0 {
		t.Fatalf("cancelled = %d, want 0 (case-insensitive match)", report.Cancelled)
	}
	if len(cancelled) != 0 {
		t.Fatalf("cancelled ids %v", cancelled)
	}
}

func TestParseActiveRunsCapturesHeadSHA(t *testing.T) {
	page := `{"workflow_runs":[{"id":1,"head_branch":"a","head_sha":"deadbeef","event":"pull_request","status":"queued","name":"Go CI","created_at":"2026-06-04T10:00:00Z"}]}`
	runs, err := parseActiveRuns([]byte(page), "acme/app")
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(runs) != 1 || runs[0].HeadSHA != "deadbeef" {
		t.Fatalf("got %+v, want HeadSHA=deadbeef", runs)
	}
}

func TestIsAlreadyCompletedErrorReRunNotQueued(t *testing.T) {
	err := fmt.Errorf("exit status 1: gh: Cannot cancel a workflow re-run that has not yet queued. (HTTP 409)")
	if !isAlreadyCompletedError(err) {
		t.Fatal("expected 409 re-run-not-queued to count as already-completed ghost")
	}
}
