package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const fakeSystemctlOutput = `  actions.runner.acme-civm.civm-1.service        loaded active running GitHub Actions Runner (acme-civm.civm-1)
  actions.runner.acme-app.civm-cmpx.service      loaded active running GitHub Actions Runner (acme-app.civm-cmpx)
  actions.runner.other-peer.civm-peer.service    loaded active running GitHub Actions Runner (other-peer.civm-peer)
`

func TestList_ParsesAllThree(t *testing.T) {
	t.Parallel()
	o := DefaultListOptions()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(fakeSystemctlOutput), nil
	}
	items, err := List(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("len = %d, want 3", len(items))
	}
	wantRepos := map[string]string{
		"civm-1":     "acme/civm",
		"civm-cmpx":  "acme/app",
		"civm-peer": "other/peer",
	}
	for _, s := range items {
		if want := wantRepos[s.Name]; want != s.Repo {
			t.Errorf("name=%s repo=%s, want %s", s.Name, s.Repo, want)
		}
		if s.ActiveState != "active" {
			t.Errorf("%s active state = %s, want active", s.Name, s.ActiveState)
		}
		if s.SubState != "running" {
			t.Errorf("%s sub state = %s, want running", s.Name, s.SubState)
		}
	}
}

func TestList_EmptyOutput(t *testing.T) {
	t.Parallel()
	o := DefaultListOptions()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(""), nil
	}
	items, err := List(context.Background(), o)
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if len(items) != 0 {
		t.Errorf("len = %d, want 0", len(items))
	}
}

func TestList_SystemctlError_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	o := DefaultListOptions()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("systemctl not found")
	}
	items, err := List(context.Background(), o)
	if err != nil {
		t.Errorf("err = %v (esperado: erro silencioso)", err)
	}
	if len(items) != 0 {
		t.Errorf("len = %d, want 0", len(items))
	}
}

func TestList_InactiveSubstateBullet(t *testing.T) {
	t.Parallel()
	// systemctl marks failed/inactive units com "●" prefix
	in := `● actions.runner.x-y.foo.service loaded failed failed GitHub Actions Runner
○ actions.runner.x-y.bar.service loaded inactive dead GitHub Actions Runner
`
	o := DefaultListOptions()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(in), nil
	}
	items, _ := List(context.Background(), o)
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	if items[0].ActiveState != "failed" {
		t.Errorf("foo active = %s", items[0].ActiveState)
	}
	if items[1].ActiveState != "inactive" {
		t.Errorf("bar active = %s", items[1].ActiveState)
	}
}

func TestParseRunnerUnit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		unit     string
		wantRepo string
		wantName string
	}{
		{
			"actions.runner.acme-civm.civm-1.service",
			"acme/civm",
			"civm-1",
		},
		{
			"actions.runner.owner-deep-repo-name.runner-name.service",
			"owner/deep-repo-name",
			"runner-name",
		},
		{
			"actions.runner.justOne.name.service",
			"justOne",
			"name",
		},
		{
			"not-a-runner.service",
			"",
			"",
		},
	}
	for _, c := range cases {
		gotRepo, gotName := parseRunnerUnit(c.unit)
		if gotRepo != c.wantRepo || gotName != c.wantName {
			t.Errorf("parseRunnerUnit(%q) = (%q, %q), want (%q, %q)",
				c.unit, gotRepo, gotName, c.wantRepo, c.wantName)
		}
	}
}

func TestRenderListTable_NonEmpty(t *testing.T) {
	t.Parallel()
	items := []Status{
		{
			UnitName:    "actions.runner.foo-bar.runner-1.service",
			Repo:        "foo/bar",
			Name:        "runner-1",
			ActiveState: "active",
			SubState:    "running",
		},
	}
	var buf bytes.Buffer
	RenderListTable(items, &buf)
	out := buf.String()
	for _, want := range []string{"RUNNER", "foo/bar", "active", "runner-1", "Total: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderListTable omitiu %q", want)
		}
	}
}

func TestRenderListTable_Empty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	RenderListTable(nil, &buf)
	if !strings.Contains(buf.String(), "Nenhum runner") {
		t.Errorf("output sem mensagem 'Nenhum runner'")
	}
	if !strings.Contains(buf.String(), "civmctl runner add") {
		t.Errorf("output sem hint civmctl runner add")
	}
}

func TestRenderListJSON_StructValid(t *testing.T) {
	t.Parallel()
	items := []Status{{Name: "x", Repo: "a/b"}}
	var buf bytes.Buffer
	if err := RenderListJSON(items, &buf); err != nil {
		t.Fatalf("err = %v", err)
	}
	var parsed struct {
		Count   int      `json:"count"`
		Runners []Status `json:"runners"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output nao e JSON valido: %v", err)
	}
	if parsed.Count != 1 {
		t.Errorf("count = %d, want 1", parsed.Count)
	}
	if len(parsed.Runners) != 1 || parsed.Runners[0].Name != "x" {
		t.Errorf("runners = %+v", parsed.Runners)
	}
}

func TestRenderListJSON_Empty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderListJSON(nil, &buf); err != nil {
		t.Fatalf("err = %v", err)
	}
	var parsed struct {
		Count   int      `json:"count"`
		Runners []Status `json:"runners"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output nao e JSON valido: %v", err)
	}
	if parsed.Count != 0 {
		t.Errorf("count = %d, want 0", parsed.Count)
	}
}

func TestParseSystemctlList_SkipShortLines(t *testing.T) {
	t.Parallel()
	in := "abc def\nactions.runner.x-y.foo.service loaded active running ok\n\n"
	got := parseSystemctlList(in)
	if len(got) != 1 {
		t.Errorf("len = %d, want 1 (descartar linha curta)", len(got))
	}
}
