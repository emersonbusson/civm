package reversewatchdog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCheck_OK_RecentFire(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.MaxAge = 2 * time.Hour
	now := time.Now().UTC()
	recent := now.Add(-30 * time.Minute).Format("Mon 2006-01-02 15:04:05 MST")
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("LastTriggerUSec=" + recent + "\n"), nil
	}
	r := Check(context.Background(), o)
	if r.Status != StatusOK {
		t.Errorf("Status = %v, want OK (last fire 30m ago, threshold 2h)", r.Status)
	}
	if r.Status.ExitCode() != 0 {
		t.Errorf("exit = %d, want 0", r.Status.ExitCode())
	}
}

func TestCheck_Stale_OldFire(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.MaxAge = 2 * time.Hour
	now := time.Now().UTC()
	old := now.Add(-5 * time.Hour).Format("Mon 2006-01-02 15:04:05 MST")
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("LastTriggerUSec=" + old + "\n"), nil
	}
	r := Check(context.Background(), o)
	if r.Status != StatusStale {
		t.Errorf("Status = %v, want Stale (last fire 5h ago, threshold 2h)", r.Status)
	}
	if r.Status.ExitCode() != 1 {
		t.Errorf("exit = %d, want 1", r.Status.ExitCode())
	}
}

func TestCheck_Unknown_NeverFired(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("LastTriggerUSec=n/a\n"), nil
	}
	r := Check(context.Background(), o)
	if r.Status != StatusUnknown {
		t.Errorf("Status = %v, want Unknown", r.Status)
	}
	if r.Status.ExitCode() != 2 {
		t.Errorf("exit = %d, want 2", r.Status.ExitCode())
	}
}

func TestCheck_Unknown_ZeroValue(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("LastTriggerUSec=0\n"), nil
	}
	r := Check(context.Background(), o)
	if r.Status != StatusUnknown {
		t.Errorf("Status = %v, want Unknown (LastTrigger=0)", r.Status)
	}
}

func TestCheck_SystemctlError(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("systemctl: not found")
	}
	r := Check(context.Background(), o)
	if r.Status != StatusUnknown {
		t.Errorf("Status = %v, want Unknown", r.Status)
	}
	if r.Err == nil {
		t.Errorf("esperava erro propagado")
	}
}

func TestCheck_InvalidUnit(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.Unit = "../bad.service"
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatalf("RunFn nao deveria ser chamado para unit invalida")
		return nil, nil
	}
	r := Check(context.Background(), o)
	if r.Status != StatusUnknown {
		t.Errorf("Status = %v, want Unknown", r.Status)
	}
	if r.Err == nil {
		t.Errorf("esperava erro de validacao")
	}
}

func TestParseLastTriggerUSec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid UTC", "LastTriggerUSec=Sun 2026-05-10 15:00:25 UTC", true},
		{"empty", "LastTriggerUSec=", false},
		{"n/a", "LastTriggerUSec=n/a", false},
		{"zero", "LastTriggerUSec=0", false},
		{"no prefix", "Other=value", false},
		{"malformed", "LastTriggerUSec=not a date", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := parseLastTriggerUSec(c.in)
			if ok != c.want {
				t.Errorf("parseLastTriggerUSec(%q) ok=%v, want %v", c.in, ok, c.want)
			}
		})
	}
}

func TestRoundDur(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
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

func TestStatusString(t *testing.T) {
	t.Parallel()
	cases := map[Status]string{
		StatusOK:      "ok",
		StatusStale:   "stale",
		StatusUnknown: "unknown",
		Status(99):    "?",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestRender_OK(t *testing.T) {
	t.Parallel()
	r := Result{Unit: "x.service", MaxAge: 2 * time.Hour, Status: StatusOK, LastFireAgo: "30m"}
	var buf bytes.Buffer
	r.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, "ok") || !strings.Contains(out, "30m") {
		t.Errorf("Render(OK) sem ok ou 30m")
	}
}

func TestRender_Stale(t *testing.T) {
	t.Parallel()
	r := Result{Unit: "x.service", MaxAge: 2 * time.Hour, Status: StatusStale, LastFireAgo: "5h"}
	var buf bytes.Buffer
	r.Render(&buf)
	out := buf.String()
	for _, want := range []string{"stale", "ATENCAO", "systemctl status", "journalctl"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render(Stale) omitiu %q", want)
		}
	}
}

func TestRender_Unknown(t *testing.T) {
	t.Parallel()
	r := Result{Unit: "x.service", MaxAge: 2 * time.Hour, Status: StatusUnknown, LastFireAgo: "never"}
	var buf bytes.Buffer
	r.Render(&buf)
	if !strings.Contains(buf.String(), "Instalar") {
		t.Errorf("Render(Unknown) sem hint Instalar")
	}
}

// fmt is used in test fixtures (importing for compile-only).
var _ = fmt.Sprint
