package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func mockRunFn(stdout []byte, err error) func(context.Context, string, ...string) ([]byte, error) {
	return func(context.Context, string, ...string) ([]byte, error) {
		return stdout, err
	}
}

func mkRuns(now time.Time, durations []time.Duration, conclusions []string) []byte {
	n := len(durations)
	runs := make([]Run, n)
	for i := 0; i < n; i++ {
		runs[i] = Run{
			DatabaseID: int64(1000 + i),
			Status:     "completed",
			Conclusion: conclusions[i],
			StartedAt:  now.Add(-time.Duration(i+1) * time.Hour),
			UpdatedAt:  now.Add(-time.Duration(i+1)*time.Hour + durations[i]),
		}
	}
	b, _ := json.Marshal(runs)
	return b
}

func validOpts() Options {
	o := DefaultOptions()
	o.Repo = "other/test"
	return o
}

func TestDetect_OK_RecentRunsCompleteNormally(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	stdout := mkRuns(now,
		[]time.Duration{2 * time.Minute, 3 * time.Minute, 5 * time.Minute},
		[]string{"success", "success", "success"})
	o := validOpts()
	o.RunFn = mockRunFn(stdout, nil)
	status, _, err := Detect(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK", status)
	}
	if status.ExitCode() != 0 {
		t.Errorf("exit = %d, want 0", status.ExitCode())
	}
}

func TestDetect_Blocked_AllFailureUnder10s(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	stdout := mkRuns(now,
		[]time.Duration{3 * time.Second, 5 * time.Second, 7 * time.Second},
		[]string{"failure", "failure", "failure"})
	o := validOpts()
	o.RunFn = mockRunFn(stdout, nil)
	status, runs, err := Detect(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != StatusBlocked {
		t.Errorf("status = %v, want Blocked", status)
	}
	if status.ExitCode() != 1 {
		t.Errorf("exit = %d, want 1", status.ExitCode())
	}
	if len(runs) != 3 {
		t.Errorf("len(runs) = %d, want 3", len(runs))
	}
}

func TestDetect_OK_OneFailureLong(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	// Run mais recente: failure mas demorou 30s (build real falhou, NAO billing)
	stdout := mkRuns(now,
		[]time.Duration{30 * time.Second, 5 * time.Second, 7 * time.Second},
		[]string{"failure", "failure", "failure"})
	o := validOpts()
	o.RunFn = mockRunFn(stdout, nil)
	status, _, _ := Detect(context.Background(), o)
	if status != StatusOK {
		t.Errorf("status = %v, want OK (run lento != billing block)", status)
	}
}

func TestDetect_OK_OneSuccess(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	stdout := mkRuns(now,
		[]time.Duration{3 * time.Second, 5 * time.Second, 5 * time.Second},
		[]string{"failure", "success", "failure"})
	o := validOpts()
	o.RunFn = mockRunFn(stdout, nil)
	status, _, _ := Detect(context.Background(), o)
	if status != StatusOK {
		t.Errorf("status = %v, want OK (1 success entre os recentes)", status)
	}
}

func TestDetect_Unknown_TooFewRuns(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	stdout := mkRuns(now,
		[]time.Duration{5 * time.Second, 5 * time.Second},
		[]string{"failure", "failure"})
	o := validOpts()
	o.RunFn = mockRunFn(stdout, nil)
	status, _, _ := Detect(context.Background(), o)
	if status != StatusUnknown {
		t.Errorf("status = %v, want Unknown (apenas 2 runs)", status)
	}
	if status.ExitCode() != 2 {
		t.Errorf("exit = %d, want 2", status.ExitCode())
	}
}

func TestDetect_Unknown_SkipsZeroStartedAt(t *testing.T) {
	t.Parallel()
	// Runs com startedAt zero (queue, never started) sao ignorados
	runs := []Run{
		{DatabaseID: 1, Status: "queued"}, // startedAt zero
		{DatabaseID: 2, Status: "queued"},
		{DatabaseID: 3, Status: "queued"},
	}
	stdout, _ := json.Marshal(runs)
	o := validOpts()
	o.RunFn = mockRunFn(stdout, nil)
	status, _, _ := Detect(context.Background(), o)
	if status != StatusUnknown {
		t.Errorf("status = %v, want Unknown (nenhum run started)", status)
	}
}

func TestDetect_GhError(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.RunFn = mockRunFn(nil, errors.New("gh: auth required"))
	status, _, err := Detect(context.Background(), o)
	if err == nil {
		t.Errorf("esperava erro propagado")
	}
	if status != StatusUnknown {
		t.Errorf("status = %v, want Unknown", status)
	}
}

func TestDetect_ParseError(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.RunFn = mockRunFn([]byte("not-json{"), nil)
	status, _, err := Detect(context.Background(), o)
	if err == nil {
		t.Errorf("esperava erro de parse")
	}
	if status != StatusUnknown {
		t.Errorf("status = %v, want Unknown", status)
	}
}

func TestValidate_RequiredFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*Options)
	}{
		{"no repo", func(o *Options) { o.Repo = "" }},
		{"bad repo", func(o *Options) { o.Repo = "norepo" }},
		{"bad workflow", func(o *Options) { o.WorkflowFile = "../ci.yml" }},
		{"low limit", func(o *Options) { o.Limit = 1 }},
		{"low min-blocked", func(o *Options) { o.MinBlocked = 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := validOpts()
			c.mut(&o)
			if err := validateOptions(o); err == nil {
				t.Errorf("esperava erro pra %q", c.name)
			}
		})
	}
}

func TestValidate_PassesOnDefault(t *testing.T) {
	t.Parallel()
	if err := validateOptions(validOpts()); err != nil {
		t.Errorf("default falhou: %v", err)
	}
}

func TestRender_OK(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	Render(StatusOK, nil, validOpts(), &buf)
	if !strings.Contains(buf.String(), "ok") {
		t.Errorf("Render(OK) sem 'ok' string")
	}
	if !strings.Contains(buf.String(), "exit 0") {
		t.Errorf("Render sem exit 0")
	}
}

func TestRender_Blocked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	runs := []Run{{DatabaseID: 99, Conclusion: "failure", StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour + 5*time.Second)}}
	var buf bytes.Buffer
	Render(StatusBlocked, runs, validOpts(), &buf)
	for _, want := range []string{"blocked", "exit 1", "Settings", "civm"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("Render(Blocked) omitiu %q", want)
		}
	}
}

func TestRender_Unknown(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	Render(StatusUnknown, nil, validOpts(), &buf)
	for _, want := range []string{"unknown", "gh auth", "GH_TOKEN"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("Render(Unknown) omitiu %q", want)
		}
	}
}

func TestRenderJSON_StructValid(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderJSON(StatusBlocked, []Run{{DatabaseID: 1}}, validOpts(), &buf); err != nil {
		t.Fatalf("err = %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output nao e JSON valido: %v", err)
	}
	if parsed["status"] != "blocked" {
		t.Errorf("status field = %v", parsed["status"])
	}
	if parsed["exit_code"].(float64) != 1 {
		t.Errorf("exit_code = %v", parsed["exit_code"])
	}
}

func TestExitCode_AllStatuses(t *testing.T) {
	t.Parallel()
	cases := map[Status]int{
		StatusOK:          0,
		StatusBlocked:     1,
		StatusUnknown:     2,
		Status("garbage"): 2,
	}
	for s, want := range cases {
		if got := s.ExitCode(); got != want {
			t.Errorf("Status(%q).ExitCode() = %d, want %d", s, got, want)
		}
	}
}

func TestDetect_Threshold_Custom(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	// Threshold=1s: runs de 5s seriam considerados "lentos" → OK, não blocked
	stdout := mkRuns(now,
		[]time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second},
		[]string{"failure", "failure", "failure"})
	o := validOpts()
	o.Threshold = 1 * time.Second
	o.RunFn = mockRunFn(stdout, nil)
	status, _, _ := Detect(context.Background(), o)
	if status != StatusOK {
		t.Errorf("status = %v, want OK com threshold=1s", status)
	}
}

func TestDetect_MinBlocked_Custom(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	// MinBlocked=2: 2 runs failure <10s já basta
	stdout := mkRuns(now,
		[]time.Duration{3 * time.Second, 5 * time.Second},
		[]string{"failure", "failure"})
	o := validOpts()
	o.MinBlocked = 2
	o.RunFn = mockRunFn(stdout, nil)
	status, _, _ := Detect(context.Background(), o)
	if status != StatusBlocked {
		t.Errorf("status = %v, want Blocked com MinBlocked=2", status)
	}
}

// TestDetect_ContextDeadlineExceeded — gh subprocess pode estourar timeout
// quando rede está degradada; o erro deve ser propagado wrapped para
// callers identificarem a causa via errors.Is.
func TestDetect_ContextDeadlineExceeded(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.RunFn = mockRunFn(nil, context.DeadlineExceeded)
	status, _, err := Detect(context.Background(), o)
	if err == nil {
		t.Fatal("expected error from timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected wrapped DeadlineExceeded, got %v", err)
	}
	if status != StatusUnknown {
		t.Errorf("status = %v, want Unknown on timeout", status)
	}
}

// TestDetect_EmptyJSONArray — gh retorna [] (sem runs); deve cair em
// StatusUnknown (regra existente: tooFewRuns), não em error.
func TestDetect_EmptyJSONArray(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.RunFn = mockRunFn([]byte("[]"), nil)
	status, runs, err := Detect(context.Background(), o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != StatusUnknown {
		t.Errorf("status = %v, want Unknown for empty array", status)
	}
	if len(runs) != 0 {
		t.Errorf("len(runs) = %d, want 0", len(runs))
	}
}

// TestDetect_MalformedJSON_NonArray — gh retorna JSON válido mas com shape
// errado (object onde array é esperado). Deve falhar com parse error
// específico, não panic.
func TestDetect_MalformedJSON_NonArray(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.RunFn = mockRunFn([]byte(`{"unexpected": "shape"}`), nil)
	_, _, err := Detect(context.Background(), o)
	if err == nil {
		t.Fatal("expected parse error for non-array JSON")
	}
	if !strings.Contains(err.Error(), "parse gh output") {
		t.Errorf("expected parse error message, got %v", err)
	}
}

// TestDetect_TruncatedJSON — buffer truncado no meio da string (cenário real
// quando gh é killado mid-output por OOM). Deve retornar parse error claro.
func TestDetect_TruncatedJSON(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.RunFn = mockRunFn([]byte(`[{"databaseId": 1, "status": "comp`), nil)
	_, _, err := Detect(context.Background(), o)
	if err == nil {
		t.Fatal("expected parse error for truncated JSON")
	}
}
