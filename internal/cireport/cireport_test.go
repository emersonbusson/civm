package cireport

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func validOpts() Options {
	o := DefaultOptions()
	o.Repo = "acme/civm"
	o.SHA = "abcd1234efgh5678ijkl9012mnop3456qrst7890"
	o.State = StateSuccess
	o.Context = "civm fallback"
	return o
}

func TestPost_BuildsCorrectGhArgs(t *testing.T) {
	t.Parallel()
	var capturedArgs []string
	o := validOpts()
	o.Description = "all gates green"
	o.TargetURL = "https://example.com/build/123"
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		capturedArgs = append([]string{name}, args...)
		return []byte(`{"id":99}`), nil
	}
	out, err := Post(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(out) != `{"id":99}` {
		t.Errorf("out = %s", out)
	}
	wantSubstr := []string{"gh", "api", "POST", "/repos/acme/civm/statuses/abcd",
		"state=success", "context=civm fallback", "description=all gates green",
		"target_url=https://example.com/build/123"}
	joined := strings.Join(capturedArgs, " ")
	for _, want := range wantSubstr {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q omitiu %q", joined, want)
		}
	}
}

func TestPost_WithoutOptionalFields(t *testing.T) {
	t.Parallel()
	var capturedArgs []string
	o := validOpts()
	// no Description, no TargetURL
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		capturedArgs = args
		return nil, nil
	}
	if _, err := Post(context.Background(), o); err != nil {
		t.Fatalf("err = %v", err)
	}
	joined := strings.Join(capturedArgs, " ")
	if strings.Contains(joined, "description=") {
		t.Errorf("args contem description quando não foi setada: %s", joined)
	}
	if strings.Contains(joined, "target_url=") {
		t.Errorf("args contem target_url quando não foi setada: %s", joined)
	}
}

func TestPost_GhError(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"message":"Not Found"}`), errors.New("gh: 404")
	}
	_, err := Post(context.Background(), o)
	if err == nil {
		t.Errorf("esperava erro propagado")
	}
}

func TestValidate_Required(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*Options)
	}{
		{"no repo", func(o *Options) { o.Repo = "" }},
		{"bad repo", func(o *Options) { o.Repo = "norepo" }},
		{"no sha", func(o *Options) { o.SHA = "" }},
		{"invalid state", func(o *Options) { o.State = State("garbage") }},
		{"no context", func(o *Options) { o.Context = "" }},
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

func TestValidate_AllFourStates(t *testing.T) {
	t.Parallel()
	cases := []State{StateSuccess, StateFailure, StatePending, StateError}
	for _, s := range cases {
		t.Run(string(s), func(t *testing.T) {
			o := validOpts()
			o.State = s
			if err := validateOptions(o); err != nil {
				t.Errorf("state %q deveria ser valido: %v", s, err)
			}
		})
	}
}

func TestState_ValidGarbage(t *testing.T) {
	t.Parallel()
	if State("garbage").Valid() {
		t.Errorf("State(garbage).Valid() = true, want false")
	}
	if !StateSuccess.Valid() {
		t.Errorf("StateSuccess.Valid() = false")
	}
}

func TestPost_DescriptionTruncated(t *testing.T) {
	t.Parallel()
	var capturedArgs []string
	o := validOpts()
	o.Description = strings.Repeat("a", 200) // > 140
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		capturedArgs = args
		return nil, nil
	}
	if _, err := Post(context.Background(), o); err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, a := range capturedArgs {
		if strings.HasPrefix(a, "description=") {
			val := strings.TrimPrefix(a, "description=")
			if len(val) > 140 {
				t.Errorf("description nao truncated: %d chars", len(val))
			}
		}
	}
}

func TestRender_Snapshot(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.Description = "manual report"
	o.TargetURL = "https://x.example.com/log"
	var buf bytes.Buffer
	Render(o, []byte(`{}`), &buf)
	out := buf.String()
	for _, want := range []string{"acme/civm", "abcd", "success", "civm fallback",
		"manual report", "x.example.com/log", "Statuses API", "github.com/acme/civm/commit"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render omitiu %q", want)
		}
	}
}

func TestRender_NoOptionals(t *testing.T) {
	t.Parallel()
	o := validOpts()
	var buf bytes.Buffer
	Render(o, nil, &buf)
	if strings.Contains(buf.String(), "Description:") {
		t.Errorf("Render mostrou Description quando não foi setada")
	}
	if strings.Contains(buf.String(), "Target URL:") {
		t.Errorf("Render mostrou Target URL quando não foi setado")
	}
}

// TestPost_ContextDeadlineExceeded — gh api pode estourar timeout em
// rede degradada; o erro precisa ser propagado wrapped para callers
// fazerem errors.Is.
func TestPost_ContextDeadlineExceeded(t *testing.T) {
	t.Parallel()
	opts := validOpts()
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	_, err := Post(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error from timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected wrapped DeadlineExceeded, got %v", err)
	}
}

// TestPost_GhAuthFailure — gh sem PAT cai em erro de auth; precisa
// preservar o output do gh para diagnóstico operacional.
func TestPost_GhAuthFailure(t *testing.T) {
	t.Parallel()
	opts := validOpts()
	authErr := errors.New("gh: must run gh auth login")
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("auth required"), authErr
	}
	out, err := Post(context.Background(), opts)
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !errors.Is(err, authErr) {
		t.Errorf("expected wrapped auth err, got %v", err)
	}
	if string(out) != "auth required" {
		t.Errorf("expected gh output preserved, got %q", out)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "ab..." {
		t.Errorf("truncate long = %q (want 'ab...')", got)
	}
	if got := truncate("abcdefghij", 8); got != "abcde..." {
		t.Errorf("truncate 8 = %q (want 'abcde...')", got)
	}
	if got := truncate("abcde", 3); got != "abc" {
		t.Errorf("truncate small n = %q", got)
	}
	if len(truncate(strings.Repeat("a", 200), 140)) > 140 {
		t.Errorf("truncate(200, 140) > 140 chars")
	}
}
