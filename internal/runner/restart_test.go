package runner

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/idle"
)

func newRestartRunner(commands map[string][]byte, errs map[string]error) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name
		if len(args) > 0 {
			key = name + " " + strings.Join(args, " ")
		}
		if err, ok := errs[key]; ok {
			return nil, err
		}
		if r, ok := commands[key]; ok {
			return r, nil
		}
		// fallback: empty success
		return nil, nil
	}
}

const fakeListOutput = `actions.runner.acme-civm.civm-1.service        loaded active running GitHub Actions Runner
actions.runner.acme-acme.civm-cmpx.service loaded active running GitHub Actions Runner
actions.runner.other-peer.civm-peer.service    loaded active running GitHub Actions Runner
`

func TestRestart_DryRun_ResolvesUnit(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Short = "civm-cmpx"
	o.RunFn = newRestartRunner(map[string][]byte{
		"systemctl list-units --type=service --no-pager --no-legend --all actions.runner.*": []byte(fakeListOutput),
	}, nil)
	r, err := Restart(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Err != nil {
		t.Fatalf("r.Err = %v", r.Err)
	}
	want := "actions.runner.acme-acme.civm-cmpx.service"
	if r.UnitResolved != want {
		t.Errorf("UnitResolved = %q, want %q", r.UnitResolved, want)
	}
	if r.RestartedOK {
		t.Errorf("RestartedOK = true em dry-run")
	}
}

func TestRestart_Execute_HappyPath(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Short = "civm-1"
	o.Execute = true
	o.IdleProbeDelay = 0
	o.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }
	o.SleepFn = func(time.Duration) {} // no real sleep in tests
	calls := []string{}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		calls = append(calls, key)
		if strings.Contains(key, "list-units") {
			return []byte(fakeListOutput), nil
		}
		if strings.Contains(key, "is-active") {
			return []byte("active\n"), nil
		}
		return nil, nil
	}
	r, err := Restart(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Err != nil {
		t.Errorf("r.Err = %v", r.Err)
	}
	if !r.RestartedOK {
		t.Errorf("RestartedOK = false")
	}
	if !r.ActiveAfter {
		t.Errorf("ActiveAfter = false")
	}
	hasRestart := false
	for _, c := range calls {
		if strings.Contains(c, "systemctl restart") {
			hasRestart = true
		}
	}
	if !hasRestart {
		t.Errorf("nenhum systemctl restart chamado")
	}
}

func TestRestart_Execute_RestartFails(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Short = "civm-1"
	o.Execute = true
	o.IdleProbeDelay = 0
	o.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }
	o.SleepFn = func(time.Duration) {}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if strings.Contains(key, "list-units") {
			return []byte(fakeListOutput), nil
		}
		if strings.Contains(key, "systemctl restart") {
			return nil, errors.New("permission denied")
		}
		return nil, nil
	}
	r, _ := Restart(context.Background(), o)
	if r.Err == nil {
		t.Errorf("esperava erro propagado de systemctl restart")
	}
	if r.RestartedOK {
		t.Errorf("RestartedOK = true mesmo com erro")
	}
}

func TestRestart_ExecuteBlocksWhenHostBusy(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Short = "civm-1"
	o.Execute = true
	o.IdleProbeDelay = 0
	o.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return []idle.Activity{{PID: 99, Command: "Runner.Worker run"}}, nil
	}
	calls := []string{}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		calls = append(calls, key)
		if strings.Contains(key, "list-units") {
			return []byte(fakeListOutput), nil
		}
		return nil, nil
	}
	r, _ := Restart(context.Background(), o)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "host nao esta ocioso") {
		t.Fatalf("Err = %v, want busy guard", r.Err)
	}
	for _, c := range calls {
		if strings.Contains(c, "systemctl restart") {
			t.Fatalf("systemctl restart called despite busy host: %v", calls)
		}
	}
}

func TestRestart_Execute_NotActiveAfter(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Short = "civm-1"
	o.Execute = true
	o.IdleProbeDelay = 0
	o.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }
	o.SleepFn = func(time.Duration) {}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if strings.Contains(key, "list-units") {
			return []byte(fakeListOutput), nil
		}
		if strings.Contains(key, "is-active") {
			return []byte("activating\n"), nil // ainda subindo
		}
		return nil, nil
	}
	r, _ := Restart(context.Background(), o)
	if r.Err == nil {
		t.Errorf("esperava erro porque nao voltou active")
	}
	if r.ActiveAfter {
		t.Errorf("ActiveAfter = true com is-active=activating")
	}
}

func TestRestart_NoMatch(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Short = "nonexistent"
	o.RunFn = newRestartRunner(map[string][]byte{
		"systemctl list-units --type=service --no-pager --no-legend --all actions.runner.*": []byte(fakeListOutput),
	}, nil)
	r, _ := Restart(context.Background(), o)
	if r.Err == nil {
		t.Errorf("esperava erro 'nenhum runner'")
	}
	if !strings.Contains(r.Err.Error(), "nenhum runner") {
		t.Errorf("err = %v, want contains 'nenhum runner'", r.Err)
	}
}

func TestRestart_AmbiguousMatch(t *testing.T) {
	t.Parallel()
	// dois units com mesmo .Short
	dupOut := `actions.runner.foo-bar.runner-1.service loaded active running x
actions.runner.baz-qux.runner-1.service loaded active running x
`
	o := DefaultRestartOptions()
	o.Short = "runner-1"
	o.RunFn = newRestartRunner(map[string][]byte{
		"systemctl list-units --type=service --no-pager --no-legend --all actions.runner.*": []byte(dupOut),
	}, nil)
	r, _ := Restart(context.Background(), o)
	if r.Err == nil {
		t.Errorf("esperava erro 'ambiguo'")
	}
	if !strings.Contains(r.Err.Error(), "ambiguo") {
		t.Errorf("err = %v, want contains 'ambiguo'", r.Err)
	}
}

func TestRestart_ExplicitUnit(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Unit = "actions.runner.x-y.foo.service"
	o.RunFn = newRestartRunner(nil, nil) // list nao deveria ser chamado
	r, err := Restart(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.UnitResolved != o.Unit {
		t.Errorf("UnitResolved = %q, want %q", r.UnitResolved, o.Unit)
	}
}

func TestValidateRestart_RequiresShortOrUnit(t *testing.T) {
	t.Parallel()
	if err := validateRestartOptions(RestartOptions{}); err == nil {
		t.Errorf("esperava erro com ambos vazios")
	}
	if err := validateRestartOptions(RestartOptions{Short: "x"}); err != nil {
		t.Errorf("Short presente nao deveria erro: %v", err)
	}
	if err := validateRestartOptions(RestartOptions{Unit: "x.service"}); err != nil {
		t.Errorf("Unit presente nao deveria erro: %v", err)
	}
	if err := validateRestartOptions(RestartOptions{Short: "x/y"}); err == nil {
		t.Errorf("esperava erro para short inseguro")
	}
	if err := validateRestartOptions(RestartOptions{Unit: "../x.service"}); err == nil {
		t.Errorf("esperava erro para unit insegura")
	}
}

func TestRestart_DefaultsApplied(t *testing.T) {
	t.Parallel()
	o := RestartOptions{
		Short: "civm-1",
		RunFn: newRestartRunner(map[string][]byte{
			"systemctl list-units --type=service --no-pager --no-legend --all actions.runner.*": []byte(fakeListOutput),
		}, nil),
	}
	// VerifyDelay zero, SleepFn nil — defaults aplicados
	r, err := Restart(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.UnitResolved == "" {
		t.Errorf("UnitResolved vazio")
	}
}

func TestRestart_DefaultRestartOptions(t *testing.T) {
	t.Parallel()
	d := DefaultRestartOptions()
	if d.Execute {
		t.Errorf("Execute default = true; deveria ser false")
	}
	if d.VerifyDelay != 3*time.Second {
		t.Errorf("VerifyDelay default = %v, want 3s", d.VerifyDelay)
	}
}

func TestRenderRestartTable_DryRun(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Short = "civm-1"
	r := RestartResult{
		UnitResolved: "actions.runner.x-y.civm-1.service",
		WouldDo:      "sudo systemctl restart ... && sleep ...",
	}
	var buf bytes.Buffer
	RenderRestartTable(r, o, &buf)
	out := buf.String()
	for _, want := range []string{"DRY-RUN", "civm-1", "(seria-aplicado)", "--execute"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render dry-run omitiu %q", want)
		}
	}
}

func TestRenderRestartTable_ExecuteSuccess(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	o.Execute = true
	r := RestartResult{UnitResolved: "x", RestartedOK: true, ActiveAfter: true}
	var buf bytes.Buffer
	RenderRestartTable(r, o, &buf)
	out := buf.String()
	if !strings.Contains(out, "EXECUTE") {
		t.Errorf("output sem EXECUTE")
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("output sem OK message")
	}
}

func TestRenderRestartTable_Error(t *testing.T) {
	t.Parallel()
	o := DefaultRestartOptions()
	r := RestartResult{Err: errors.New("boom")}
	var buf bytes.Buffer
	RenderRestartTable(r, o, &buf)
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("output sem mensagem de erro")
	}
}
