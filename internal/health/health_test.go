package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func okCollector() *Collector {
	c := NewDefaultCollector("/tmp")
	c.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 * (1 << 30), 50 * (1 << 30), nil
	}
	c.MeminfoFn = func() (int64, error) { return 8 * 1024 * 1024, nil } // 8 GB
	c.RunnerUnitsFn = func(context.Context) ([]string, error) {
		return []string{"actions.runner.foo.service"}, nil
	}
	now := time.Now().Add(-2 * time.Hour)
	c.LastCleanupFn = func(context.Context) (*time.Time, string, error) {
		return &now, "liberados 4.2 GB", nil
	}
	c.TimerStateFn = func(context.Context, string) (TimerState, error) {
		return TimerState{Enabled: "enabled", Active: "active"}, nil
	}
	c.ServiceStateFn = func(context.Context, string) (ServiceState, error) {
		return ServiceState{LoadState: "loaded", Result: "success"}, nil
	}
	return c
}

func TestCollect_AllOK(t *testing.T) {
	t.Parallel()
	c := okCollector()
	r := c.Collect(context.Background())
	if r.Exit() != 0 {
		t.Errorf("Exit = %d, want 0", r.Exit())
	}
	if len(r.Checks) != 12 {
		t.Errorf("len(Checks) = %d, want 12", len(r.Checks))
	}
}

func TestCollect_RunnerWatchdogServiceFailedIsCritical(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.ServiceStateFn = func(_ context.Context, service string) (ServiceState, error) {
		if service == "civmctl-runner-watchdog.service" {
			return ServiceState{LoadState: "loaded", Result: "exit-code"}, nil
		}
		return ServiceState{LoadState: "loaded", Result: "success"}, nil
	}

	r := c.Collect(context.Background())
	if r.Exit() != int(StatusCritical) {
		t.Fatalf("Exit = %d, want critical", r.Exit())
	}
	for _, check := range r.Checks {
		if check.Name == "SERVICE_RUNNER_WATCHDOG" &&
			check.Status == StatusCritical &&
			strings.Contains(check.Detail, "exit-code") {
			return
		}
	}
	t.Fatalf("runner watchdog service failure not reported: %+v", r.Checks)
}

func TestCollect_ReaperTimerActiveServiceFailedIsCritical(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.ServiceStateFn = func(_ context.Context, service string) (ServiceState, error) {
		if service == "civmctl-run-reaper.service" {
			return ServiceState{LoadState: "loaded", Result: "exit-code"}, nil
		}
		return ServiceState{LoadState: "loaded", Result: "success"}, nil
	}

	r := c.Collect(context.Background())

	if r.Exit() != int(StatusCritical) {
		t.Fatalf("Exit = %d, want critical", r.Exit())
	}
	found := false
	for _, ch := range r.Checks {
		if ch.Name == "SERVICE_REAPER" &&
			ch.Status == StatusCritical &&
			strings.Contains(ch.Detail, "exit-code") {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed reaper service not reported: %+v", r.Checks)
	}
}

func TestCollect_ReaperServiceMissingIsCritical(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.ServiceStateFn = func(context.Context, string) (ServiceState, error) {
		return ServiceState{LoadState: "not-found", Result: "success"}, nil
	}
	if got := c.Collect(context.Background()).Exit(); got != int(StatusCritical) {
		t.Fatalf("Exit = %d, want critical", got)
	}
}

func TestCollect_DiskWarn(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 * (1 << 30), 5 * (1 << 30), nil // 5 GB free, warn=10
	}
	r := c.Collect(context.Background())
	if r.Exit() != int(StatusWarn) {
		t.Errorf("Exit = %d, want %d (warn)", r.Exit(), StatusWarn)
	}
}

func TestCollect_DiskCritical(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 * (1 << 30), 1 * (1 << 30), nil // 1 GB free, crit=3
	}
	r := c.Collect(context.Background())
	if r.Exit() != int(StatusCritical) {
		t.Errorf("Exit = %d, want %d (crit)", r.Exit(), StatusCritical)
	}
}

func TestCollect_StatfsError(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.StatfsFn = func(string) (uint64, uint64, error) {
		return 0, 0, errors.New("statfs explodiu")
	}
	r := c.Collect(context.Background())
	if r.Exit() < int(StatusWarn) {
		t.Errorf("Exit = %d, want >= warn", r.Exit())
	}
	found := false
	for _, ch := range r.Checks {
		if ch.Name == "DISK" && strings.Contains(ch.Detail, "explodiu") {
			found = true
		}
	}
	if !found {
		t.Errorf("erro de statfs nao apareceu no detail")
	}
}

func TestCollect_MemCritical(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.MeminfoFn = func() (int64, error) { return 100 * 1024, nil } // 100 MB, crit=128
	r := c.Collect(context.Background())
	if r.Exit() != int(StatusCritical) {
		t.Errorf("Exit = %d, want crit", r.Exit())
	}
}

func TestCollect_NoRunnersIsOK(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.RunnerUnitsFn = func(context.Context) ([]string, error) { return nil, nil }
	r := c.Collect(context.Background())
	if r.Exit() != 0 {
		t.Errorf("Exit = %d, want 0 (rodar fora da VM nao e erro)", r.Exit())
	}
}

func TestCollect_NoLastCleanupIsWarn(t *testing.T) {
	t.Parallel()
	c := okCollector()
	c.LastCleanupFn = func(context.Context) (*time.Time, string, error) {
		return nil, "", nil
	}
	r := c.Collect(context.Background())
	if r.Exit() != int(StatusWarn) {
		t.Errorf("Exit = %d, want warn (sem cleanup historico)", r.Exit())
	}
}

func TestCollect_OldCleanupIsWarn(t *testing.T) {
	t.Parallel()
	c := okCollector()
	old := time.Now().Add(-72 * time.Hour)
	c.LastCleanupFn = func(context.Context) (*time.Time, string, error) {
		return &old, "liberados 1 GB", nil
	}
	r := c.Collect(context.Background())
	if r.Exit() != int(StatusWarn) {
		t.Errorf("Exit = %d, want warn (cleanup >48h)", r.Exit())
	}
}

func TestCollect_TimersMissingAndStale(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		timer     string
		state     TimerState
		err       error
		wantExit  int
		wantCheck string
	}{
		{
			name:      "cleanup missing is critical",
			timer:     "civmctl-cleanup.timer",
			err:       errors.New("not found"),
			wantExit:  int(StatusCritical),
			wantCheck: "TIMER_CLEANUP",
		},
		{
			name:      "disk stale is critical",
			timer:     "civmctl-disk-watchdog.timer",
			state:     TimerState{Enabled: "enabled", Active: "inactive"},
			wantExit:  int(StatusCritical),
			wantCheck: "TIMER_DISK",
		},
		{
			name:      "reverse missing is warning",
			timer:     "civmctl-reverse-watchdog.timer",
			err:       errors.New("not found"),
			wantExit:  int(StatusWarn),
			wantCheck: "TIMER_REVERSE",
		},
		{
			name:      "runner watchdog missing is warning",
			timer:     "civmctl-runner-watchdog.timer",
			err:       errors.New("not found"),
			wantExit:  int(StatusWarn),
			wantCheck: "TIMER_RUNNER",
		},
		{
			name:      "metrics missing is warning",
			timer:     "civmctl-metrics.timer",
			err:       errors.New("not found"),
			wantExit:  int(StatusWarn),
			wantCheck: "TIMER_METRICS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := okCollector()
			c.TimerStateFn = func(_ context.Context, timer string) (TimerState, error) {
				if timer == tt.timer {
					return tt.state, tt.err
				}
				return TimerState{Enabled: "enabled", Active: "active"}, nil
			}
			r := c.Collect(context.Background())
			if r.Exit() != tt.wantExit {
				t.Fatalf("Exit = %d, want %d", r.Exit(), tt.wantExit)
			}
			found := false
			for _, ch := range r.Checks {
				if ch.Name == tt.wantCheck && ch.Status == Status(tt.wantExit) {
					found = true
				}
			}
			if !found {
				t.Fatalf("check %s with status %d not found: %+v", tt.wantCheck, tt.wantExit, r.Checks)
			}
		})
	}
}

func TestRender_ContainsExitLine(t *testing.T) {
	t.Parallel()
	c := okCollector()
	r := c.Collect(context.Background())
	var buf bytes.Buffer
	r.Render(&buf)
	if !strings.Contains(buf.String(), "EXIT:") {
		t.Errorf("Render() omitiu EXIT line")
	}
	if !strings.Contains(buf.String(), "DISK") {
		t.Errorf("Render() omitiu DISK row")
	}
}

func TestRenderJSON_StructValid(t *testing.T) {
	t.Parallel()
	c := okCollector()
	r := c.Collect(context.Background())
	var buf bytes.Buffer
	if err := r.RenderJSON(&buf); err != nil {
		t.Fatalf("err = %v", err)
	}
	var parsed struct {
		Checks []map[string]any `json:"checks"`
		Exit   int              `json:"exit"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output nao e JSON valido: %v", err)
	}
	if len(parsed.Checks) != 12 {
		t.Errorf("Checks len = %d, want 12", len(parsed.Checks))
	}
	if parsed.Exit != 0 {
		t.Errorf("Exit = %d, want 0", parsed.Exit)
	}
}

func TestStatusString(t *testing.T) {
	t.Parallel()
	cases := map[Status]string{StatusOK: "OK", StatusWarn: "WARN", StatusCritical: "CRIT"}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", s, got, want)
		}
	}
	if got := Status(99).String(); got != "?" {
		t.Errorf("Status(99) = %q, want ?", got)
	}
}

func TestRoundDur(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{2 * time.Hour, "2h"},
		{72 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := roundDur(c.d); got != c.want {
			t.Errorf("roundDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestParseMemAvailable(t *testing.T) {
	t.Parallel()
	in := "MemTotal:       16384000 kB\nMemFree:         2048000 kB\nMemAvailable:    8192000 kB\n"
	got, err := parseMemAvailableKB(in)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 8192000 {
		t.Errorf("got %d, want 8192000", got)
	}
}

func TestParseMemAvailable_Missing(t *testing.T) {
	t.Parallel()
	if _, err := parseMemAvailableKB("MemTotal: 1 kB\n"); err == nil {
		t.Errorf("esperado erro quando MemAvailable ausente")
	}
}

func TestParseMemAvailable_Malformed(t *testing.T) {
	t.Parallel()
	if _, err := parseMemAvailableKB("MemAvailable:\n"); err == nil {
		t.Errorf("esperado erro com linha malformada")
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
	if got := truncate("abcdefghij", 5); got != "abcd…" {
		t.Errorf("got %q, want abcd…", got)
	}
}

func TestDefaultStatfs_RealDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	total, free, err := defaultStatfs(dir)
	if err != nil {
		t.Fatalf("defaultStatfs(%s) erro = %v", dir, err)
	}
	if total == 0 {
		t.Errorf("total = 0; tmpfs/local FS deveria ter espaço")
	}
	if free > total {
		t.Errorf("free %d > total %d", free, total)
	}
}

func TestDefaultStatfs_NonExistent(t *testing.T) {
	t.Parallel()
	if _, _, err := defaultStatfs("/this/path/does/not/exist/xyz"); err == nil {
		t.Errorf("esperado erro em path inexistente")
	}
}

// Tests below touch the package-global osReadFile or run real OS commands.
// They are not Parallel-safe; keep them serial.

func TestDefaultMeminfo_Real(t *testing.T) {
	kb, err := defaultMeminfo()
	if err != nil {
		t.Skipf("defaultMeminfo() falhou em ambiente nao-Linux: %v", err)
	}
	if kb <= 0 {
		t.Errorf("MemAvailable = %d; esperado > 0 em sistema Linux normal", kb)
	}
}

func TestDefaultMeminfo_BrokenFile(t *testing.T) {
	orig := osReadFile
	osReadFile = func(string) ([]byte, error) {
		return []byte("MemTotal: 1 kB\n"), nil
	}
	defer func() { osReadFile = orig }()
	if _, err := defaultMeminfo(); err == nil {
		t.Errorf("esperado erro com /proc/meminfo sem MemAvailable")
	}
}

func TestDefaultMeminfo_FileError(t *testing.T) {
	orig := osReadFile
	osReadFile = func(string) ([]byte, error) {
		return nil, errors.New("read falhou")
	}
	defer func() { osReadFile = orig }()
	if _, err := defaultMeminfo(); err == nil {
		t.Errorf("esperado propagar erro de leitura")
	}
}

func TestDefaultRunnerUnits_NonNil(t *testing.T) {
	// Em ambiente sem systemctl ou sem actions.runner.*, deve retornar nil sem erro.
	units, err := defaultRunnerUnits(context.Background())
	if err != nil {
		t.Errorf("erro inesperado: %v", err)
	}
	_ = units // pode ser nil ou vazio em dev machine
}

func TestDefaultLastCleanup_NoData(t *testing.T) {
	// Sem journalctl ou sem unit civmctl-cleanup, retorna nil sem erro.
	when, _, err := defaultLastCleanup(context.Background())
	if err != nil {
		t.Errorf("erro inesperado: %v", err)
	}
	_ = when
}

func TestDefaultLastCleanupReadsCleanupServiceUnit(t *testing.T) {
	orig := commandOutput
	defer func() { commandOutput = orig }()

	var gotName string
	var gotArgs []string
	commandOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte("2026-05-21T04:03:01+0000 gha civmctl[123]: Total freed: 20.0 kB\n"), nil
	}

	when, detail, err := defaultLastCleanup(context.Background())
	if err != nil {
		t.Fatalf("defaultLastCleanup err = %v", err)
	}
	if when == nil {
		t.Fatal("when nil; expected cleanup journal line to parse")
	}
	if gotName != "journalctl" || strings.Join(gotArgs, " ") != "-u civmctl-cleanup.service --since 7 days ago --no-pager --reverse -n 1 -o short-iso" {
		t.Fatalf("journal command = %s %s", gotName, strings.Join(gotArgs, " "))
	}
	if !strings.Contains(detail, "Total freed") {
		t.Fatalf("detail = %q, want journal suffix", detail)
	}
}

func TestDefaultLastCleanupReadsJournalShortISOWithColonOffset(t *testing.T) {
	orig := commandOutput
	defer func() { commandOutput = orig }()

	commandOutput = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("2026-05-21T04:03:59+00:00 gha systemd[1]: Finished civmctl-cleanup.service\n"), nil
	}

	when, detail, err := defaultLastCleanup(context.Background())
	if err != nil {
		t.Fatalf("defaultLastCleanup err = %v", err)
	}
	if when == nil {
		t.Fatal("when nil; expected cleanup journal line with colon offset to parse")
	}
	if got := when.UTC().Format(time.RFC3339); got != "2026-05-21T04:03:59Z" {
		t.Fatalf("when = %s", got)
	}
	if !strings.Contains(detail, "Finished civmctl-cleanup.service") {
		t.Fatalf("detail = %q, want journal suffix", detail)
	}
}
