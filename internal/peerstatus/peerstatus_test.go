package peerstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCollect_HappyPath(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.Repo = "acme/civm"
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(key, "actions/runners"):
			return []byte(`{"runners":[{"name":"civm-self","status":"online"},{"name":"civm-2","status":"offline"}]}`), nil
		case strings.Contains(key, "run list"):
			return []byte(`[{"databaseId":12345,"status":"completed","conclusion":"success","createdAt":"2026-05-10T12:00:00Z","url":"https://github.com/x/y/actions/runs/12345"}]`), nil
		case strings.Contains(key, "gh run list --repo"):
			return []byte(`[]`), nil
		default:
			// billing-status delega a billing package que invoca gh run list
			// com diferentes args. Retornar runs healthy:
			return []byte(`[
				{"databaseId":1,"conclusion":"success","startedAt":"2026-05-10T11:50:00Z","updatedAt":"2026-05-10T11:55:00Z"},
				{"databaseId":2,"conclusion":"success","startedAt":"2026-05-10T11:40:00Z","updatedAt":"2026-05-10T11:45:00Z"},
				{"databaseId":3,"conclusion":"success","startedAt":"2026-05-10T11:30:00Z","updatedAt":"2026-05-10T11:35:00Z"}
			]`), nil
		}
	}
	s, err := Collect(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if s.Repo != "acme/civm" {
		t.Errorf("Repo = %s", s.Repo)
	}
	if s.RunnersTotal != 2 {
		t.Errorf("RunnersTotal = %d", s.RunnersTotal)
	}
	if s.RunnersOnline != 1 {
		t.Errorf("RunnersOnline = %d, want 1", s.RunnersOnline)
	}
	if s.LastRun == nil {
		t.Errorf("LastRun nil")
	} else if s.LastRun.DatabaseID != 12345 {
		t.Errorf("LastRun.DatabaseID = %d", s.LastRun.DatabaseID)
	}
}

func TestCollect_RunnersOffline(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.Repo = "x/y"
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"runners":[]}`), nil
	}
	s, _ := Collect(context.Background(), o)
	if s.RunnersTotal != 0 || s.RunnersOnline != 0 {
		t.Errorf("expected 0 runners")
	}
}

func TestCollect_GhFails_Tolerant(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.Repo = "x/y"
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("gh: 401")
	}
	s, err := Collect(context.Background(), o)
	if err != nil {
		t.Errorf("err = %v (esperado: tolerante)", err)
	}
	if s.RunnersTotal != 0 {
		t.Errorf("Runners deveria ser 0 quando gh falha")
	}
	if s.LastRun != nil {
		t.Errorf("LastRun deveria ser nil quando gh falha")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		repo string
	}{
		{"empty", ""},
		{"no-slash", "noslash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := DefaultOptions()
			o.Repo = c.repo
			if err := validateOptions(o); err == nil {
				t.Errorf("esperava erro pra %q", c.repo)
			}
		})
	}
}

func TestValidate_OK(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.Repo = "owner/repo"
	if err := validateOptions(o); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestRender_Healthy(t *testing.T) {
	t.Parallel()
	s := Status{
		Repo: "x/y", WorkflowFile: "ci.yml",
		BillingStatus: "ok", BillingExitCode: 0,
		RunnersTotal: 1, RunnersOnline: 1,
		RunnerNames: []string{"civm-x"},
		LastRun: &RunSummary{
			DatabaseID: 99, Conclusion: "success",
			CreatedAt: time.Now().Add(-30 * time.Minute),
			URL:       "https://gh/x",
		},
	}
	var buf bytes.Buffer
	s.Render(&buf)
	out := buf.String()
	for _, w := range []string{"x/y", "ok", "1/1 online", "civm-x", "#99", "OK: billing OK"} {
		if !strings.Contains(out, w) {
			t.Errorf("Render omitiu %q", w)
		}
	}
}

func TestRender_BlockedNoRunners(t *testing.T) {
	t.Parallel()
	s := Status{Repo: "x/y", BillingStatus: "blocked", RunnersOnline: 0}
	var buf bytes.Buffer
	s.Render(&buf)
	if !strings.Contains(buf.String(), "ALERTA") {
		t.Errorf("Render omitiu ALERTA")
	}
}

func TestRender_BlockedWithFallback(t *testing.T) {
	t.Parallel()
	s := Status{Repo: "x/y", BillingStatus: "blocked", RunnersOnline: 1}
	var buf bytes.Buffer
	s.Render(&buf)
	if !strings.Contains(buf.String(), "fallback") {
		t.Errorf("Render omitiu mensagem fallback")
	}
}

func TestRender_NoRunners(t *testing.T) {
	t.Parallel()
	s := Status{Repo: "x/y", BillingStatus: "ok", RunnersOnline: 0}
	var buf bytes.Buffer
	s.Render(&buf)
	if !strings.Contains(buf.String(), "WARN") {
		t.Errorf("Render omitiu WARN")
	}
}

func TestRender_NoLastRun(t *testing.T) {
	t.Parallel()
	s := Status{Repo: "x/y", BillingStatus: "ok", RunnersOnline: 1, RunnersTotal: 1}
	var buf bytes.Buffer
	s.Render(&buf)
	if !strings.Contains(buf.String(), "nenhum encontrado") {
		t.Errorf("Render omitiu mensagem 'nenhum encontrado'")
	}
}

func TestRenderJSON_Valid(t *testing.T) {
	t.Parallel()
	s := Status{Repo: "x/y", BillingStatus: "ok", RunnersTotal: 2}
	var buf bytes.Buffer
	if err := s.RenderJSON(&buf); err != nil {
		t.Fatalf("err = %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output nao e JSON valido: %v", err)
	}
	if parsed["repo"] != "x/y" {
		t.Errorf("repo = %v", parsed["repo"])
	}
}

func TestStatusSeverity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Status
		want Severity
	}{
		{
			name: "ok",
			in:   Status{BillingStatus: "ok", RunnersOnline: 1},
			want: SeverityOK,
		},
		{
			name: "billing blocked but fallback runner online",
			in:   Status{BillingStatus: "blocked", RunnersOnline: 1},
			want: SeverityWarn,
		},
		{
			name: "billing unknown",
			in:   Status{BillingStatus: "unknown", RunnersOnline: 1},
			want: SeverityWarn,
		},
		{
			name: "no runners",
			in:   Status{BillingStatus: "ok", RunnersOnline: 0},
			want: SeverityWarn,
		},
		{
			name: "billing blocked and no runners",
			in:   Status{BillingStatus: "blocked", RunnersOnline: 0},
			want: SeverityCritical,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.in.Severity(); got != tt.want {
				t.Fatalf("Severity() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCollectFleetSummaryAndRenderJSON(t *testing.T) {
	t.Parallel()
	opts := DefaultFleetOptions()
	opts.Repos = []string{"acme/civm", "acme/app", "other/peer"}
	opts.RunFn = fleetRunFn(t)

	report, err := CollectFleet(context.Background(), opts)
	if err != nil {
		t.Fatalf("CollectFleet err = %v", err)
	}
	if report.Exit != 2 {
		t.Fatalf("Exit = %d, want 2", report.Exit)
	}
	if report.Summary.Total != 3 || report.Summary.OK != 1 || report.Summary.Warn != 1 || report.Summary.Critical != 1 {
		t.Fatalf("summary counts = %+v", report.Summary)
	}
	if report.Summary.RunnersOnline != 2 || report.Summary.RunnersTotal != 2 {
		t.Fatalf("runner summary = %+v", report.Summary)
	}
	severities := map[string]Severity{}
	for _, peer := range report.Peers {
		severities[peer.Repo] = peer.Severity
		if peer.LastRun == nil {
			t.Fatalf("peer %s missing last run", peer.Repo)
		}
	}
	if severities["acme/civm"] != SeverityOK {
		t.Fatalf("acme/civm severity = %s", severities["acme/civm"])
	}
	if severities["acme/app"] != SeverityWarn {
		t.Fatalf("acme/app severity = %s", severities["acme/app"])
	}
	if severities["other/peer"] != SeverityCritical {
		t.Fatalf("peer severity = %s", severities["other/peer"])
	}

	var buf bytes.Buffer
	if err := report.RenderJSON(&buf); err != nil {
		t.Fatalf("RenderJSON err = %v", err)
	}
	var parsed FleetReport
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed.Summary.Critical != 1 || len(parsed.Peers) != 3 {
		t.Fatalf("parsed fleet = %+v", parsed)
	}
}

func TestRenderFleetHuman(t *testing.T) {
	t.Parallel()
	report := FleetReport{
		WorkflowFile: "ci.yml",
		Summary: FleetSummary{
			Total: 2, OK: 1, Warn: 1, Critical: 0,
			RunnersOnline: 1, RunnersTotal: 2,
		},
		Peers: []FleetPeer{
			{
				Status: Status{
					Repo: "acme/civm", WorkflowFile: "ci.yml",
					BillingStatus: "ok", RunnersOnline: 1, RunnersTotal: 1,
					LastRun: &RunSummary{DatabaseID: 10, Conclusion: "success", URL: "https://github.com/emersonbusson/civm/actions/runs/10"},
				},
				Severity: SeverityOK,
			},
			{
				Status: Status{
					Repo: "acme/app", WorkflowFile: "ci.yml",
					BillingStatus: "unknown", RunnersOnline: 0, RunnersTotal: 1,
				},
				Severity: SeverityWarn,
			},
		},
		Exit: 1,
	}
	var buf bytes.Buffer
	report.Render(&buf)
	for _, want := range []string{"read-only", "ok=1 warn=1 critical=0", "acme/civm", "acme/app", "WARN"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("Render omitted %q:\n%s", want, buf.String())
		}
	}
}

func TestCollectFleetRejectsEmptyRepos(t *testing.T) {
	t.Parallel()
	opts := DefaultFleetOptions()
	opts.Repos = nil
	if _, err := CollectFleet(context.Background(), opts); err == nil {
		t.Fatalf("expected empty repos validation error")
	}
}

func TestRender_LastRunNoConclusion(t *testing.T) {
	t.Parallel()
	s := Status{
		Repo:          "x/y",
		BillingStatus: "ok",
		RunnersOnline: 1, RunnersTotal: 1,
		LastRun: &RunSummary{
			DatabaseID: 1, Status: "in_progress", Conclusion: "",
			CreatedAt: time.Now().Add(-time.Minute),
		},
	}
	var buf bytes.Buffer
	s.Render(&buf)
	if !strings.Contains(buf.String(), "in_progress") {
		t.Errorf("Render deveria mostrar status quando Conclusion vazio")
	}
}

// TestCollect_TimeoutDoesNotCrash — todas as chamadas gh estouram timeout;
// peerstatus deve degradar graciosamente (sem error), com campos zero.
// Peer-status é tolerante por design: melhor mostrar "unknown" para um
// repo do que abortar a coleta inteira em multi-peer view.
func TestCollect_TimeoutDoesNotCrash(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.Repo = "acme/civm"
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	s, err := Collect(context.Background(), o)
	if err != nil {
		t.Fatalf("Collect must tolerate gh timeouts, got err=%v", err)
	}
	if s.Repo != "acme/civm" {
		t.Errorf("Repo not preserved on timeout: %s", s.Repo)
	}
	if s.RunnersTotal != 0 || s.LastRun != nil {
		t.Errorf("expected zero fields on full timeout, got %+v", s)
	}
}

// TestCollect_MalformedRunnersJSON — gh retorna JSON malformado no endpoint
// /actions/runners; peerstatus deve ignorar (tolerante) e continuar com
// outros endpoints.
func TestCollect_MalformedRunnersJSON(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.Repo = "acme/civm"
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if strings.Contains(key, "actions/runners") {
			return []byte("not-json{{{"), nil
		}
		// Outros endpoints retornam vazio para isolar o teste no runners.
		return []byte("[]"), nil
	}
	s, err := Collect(context.Background(), o)
	if err != nil {
		t.Fatalf("Collect must tolerate malformed JSON, got err=%v", err)
	}
	if s.RunnersTotal != 0 {
		t.Errorf("RunnersTotal = %d, want 0 (parse failed silently)", s.RunnersTotal)
	}
}

// TestCollect_PartialFailure — runners OK mas run list timeout. Deve
// preservar info dos runners e zerar só LastRun.
func TestCollect_PartialFailure(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.Repo = "acme/civm"
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(key, "actions/runners"):
			return []byte(`{"runners":[{"name":"civm-1","status":"online"}]}`), nil
		case strings.Contains(key, "run list"):
			return nil, errors.New("gh: rate limited")
		default:
			return []byte("[]"), nil
		}
	}
	s, err := Collect(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if s.RunnersTotal != 1 || s.RunnersOnline != 1 {
		t.Errorf("runners not preserved across partial failure: %+v", s)
	}
	if s.LastRun != nil {
		t.Errorf("LastRun should be nil on rate-limit, got %+v", s.LastRun)
	}
}

func fleetRunFn(t *testing.T) func(context.Context, string, ...string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "gh" {
			t.Fatalf("name = %q, want gh", name)
		}
		key := strings.Join(args, " ")
		switch {
		case strings.Contains(key, "/repos/acme/civm/actions/runners"):
			return []byte(`{"runners":[{"name":"civm-self","status":"online"}]}`), nil
		case strings.Contains(key, "/repos/acme/app/actions/runners"):
			return []byte(`{"runners":[{"name":"civm-app","status":"online"}]}`), nil
		case strings.Contains(key, "/repos/other/peer/actions/runners"):
			return []byte(`{"runners":[]}`), nil
		case strings.Contains(key, "--repo acme/civm") && strings.Contains(key, "startedAt"):
			return healthyBillingRuns(), nil
		case strings.Contains(key, "--repo acme/app") && strings.Contains(key, "startedAt"):
			return blockedBillingRuns(), nil
		case strings.Contains(key, "--repo other/peer") && strings.Contains(key, "startedAt"):
			return blockedBillingRuns(), nil
		case strings.Contains(key, "--repo acme/civm") && strings.Contains(key, "createdAt"):
			return lastRun(101, "success"), nil
		case strings.Contains(key, "--repo acme/app") && strings.Contains(key, "createdAt"):
			return lastRun(202, "failure"), nil
		case strings.Contains(key, "--repo other/peer") && strings.Contains(key, "createdAt"):
			return lastRun(303, "failure"), nil
		default:
			t.Fatalf("unexpected gh args: %s", key)
			return nil, nil
		}
	}
}

func healthyBillingRuns() []byte {
	return []byte(`[
		{"databaseId":1,"status":"completed","conclusion":"success","startedAt":"2026-05-10T12:00:00Z","updatedAt":"2026-05-10T12:05:00Z"},
		{"databaseId":2,"status":"completed","conclusion":"success","startedAt":"2026-05-10T11:00:00Z","updatedAt":"2026-05-10T11:05:00Z"},
		{"databaseId":3,"status":"completed","conclusion":"success","startedAt":"2026-05-10T10:00:00Z","updatedAt":"2026-05-10T10:05:00Z"}
	]`)
}

func blockedBillingRuns() []byte {
	return []byte(`[
		{"databaseId":11,"status":"completed","conclusion":"failure","startedAt":"2026-05-10T12:00:00Z","updatedAt":"2026-05-10T12:00:04Z"},
		{"databaseId":12,"status":"completed","conclusion":"failure","startedAt":"2026-05-10T11:00:00Z","updatedAt":"2026-05-10T11:00:04Z"},
		{"databaseId":13,"status":"completed","conclusion":"failure","startedAt":"2026-05-10T10:00:00Z","updatedAt":"2026-05-10T10:00:04Z"}
	]`)
}

func lastRun(id int64, conclusion string) []byte {
	return []byte(fmt.Sprintf(`[{
		"databaseId":%d,
		"status":"completed",
		"conclusion":%q,
		"createdAt":"2026-05-10T12:00:00Z",
		"url":"https://github.com/emersonbusson/civm/actions/runs/%d"
	}]`, id, conclusion, id))
}
