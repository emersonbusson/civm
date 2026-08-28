package runner

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/idle"
)

func validOpts() AddOptions {
	o := DefaultOptions()
	o.Repo = "other/peer"
	o.Token = "AAAA1234"
	o.Short = "cmpx"
	o.BaseDir = "/home/emdev"
	o.RunAsUser = "emdev"
	o.SHA256FileFn = func(string) (string, error) {
		return o.RunnerSHA256, nil
	}
	return o
}

func TestValidate_AllRequiredFieldsChecked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*AddOptions)
	}{
		{"no repo", func(o *AddOptions) { o.Repo = "" }},
		{"bad repo (no slash)", func(o *AddOptions) { o.Repo = "peer" }},
		{"no token", func(o *AddOptions) { o.Token = "" }},
		{"no short", func(o *AddOptions) { o.Short = "" }},
		{"bad short", func(o *AddOptions) { o.Short = "../cmpx" }},
		{"bad label", func(o *AddOptions) { o.Label = "civm, bad label" }},
		{"no runner-version", func(o *AddOptions) { o.RunnerVersion = "" }},
		{"bad runner-version", func(o *AddOptions) { o.RunnerVersion = "2.334" }},
		{"no runner-sha256", func(o *AddOptions) { o.RunnerSHA256 = "" }},
		{"no base-dir", func(o *AddOptions) { o.BaseDir = "" }},
		{"base-dir not clean", func(o *AddOptions) { o.BaseDir = "/home/emdev/.." }},
		{"no run-as", func(o *AddOptions) { o.RunAsUser = "" }},
		{"bad run-as", func(o *AddOptions) { o.RunAsUser = "emdev;root" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := validOpts()
			c.mut(&o)
			if err := validateOptions(o); err == nil {
				t.Errorf("esperava erro para caso %q", c.name)
			}
		})
	}
}

func TestValidate_ValidPasses(t *testing.T) {
	t.Parallel()
	if err := validateOptions(validOpts()); err != nil {
		t.Errorf("opts validas falharam: %v", err)
	}
}

func TestAdd_DryRun_NoExecute(t *testing.T) {
	t.Parallel()
	called := false
	o := validOpts()
	o.Execute = false
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	results, err := Add(context.Background(), o)
	if err != nil {
		t.Fatalf("Add err = %v", err)
	}
	if called {
		t.Errorf("RunFn chamado em dry-run")
	}
	if len(results) != 7 {
		t.Errorf("len(results) = %d, want 7 steps", len(results))
	}
	for _, r := range results {
		if r.Executed {
			t.Errorf("%s Executed em dry-run", r.Name)
		}
		if r.WouldDo == "" {
			t.Errorf("%s sem WouldDo", r.Name)
		}
	}
}

func TestAdd_Execute_RunsAllSteps(t *testing.T) {
	t.Parallel()
	calls := 0
	o := validOpts()
	o.Execute = true
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	results, err := Add(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, r := range results {
		if !r.Executed {
			t.Errorf("%s nao Executed", r.Name)
		}
		if r.Err != nil {
			t.Errorf("%s err = %v", r.Name, r.Err)
		}
	}
	if calls < 6 {
		t.Errorf("calls = %d, esperava ao menos 6 comandos externos", calls)
	}
	// config.sh / svc.sh steps use `sh -c 'cd dir && …'` so cwd is correct.
}

func TestAdd_StopsOnError(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.Execute = true
	step := 0
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		step++
		if step == 2 {
			return nil, errors.New("download falhou")
		}
		return nil, nil
	}
	results, err := Add(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	hadErr := false
	executed := 0
	for _, r := range results {
		if r.Err != nil {
			hadErr = true
		}
		if r.Executed {
			executed++
		}
	}
	if !hadErr {
		t.Errorf("esperava propagar erro do download")
	}
	// mkdir success + download fail -> 1 executed
	if executed != 1 {
		t.Errorf("executed = %d, want 1 (mkdir antes do erro)", executed)
	}
}

func TestAdd_VerifiesRunnerSHA256BeforeExtract(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.Execute = true
	o.SHA256FileFn = func(string) (string, error) {
		return "bad-sha", nil
	}
	calls := []string{}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	results, err := Add(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var gotErr bool
	for _, r := range results {
		if r.Name == "verify_runner_sha256" && r.Err != nil {
			gotErr = true
		}
	}
	if !gotErr {
		t.Fatalf("verify_runner_sha256 should fail, results=%+v", results)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "tar ") || strings.Contains(call, "config.sh") {
			t.Fatalf("runner setup continued after checksum failure: %v", calls)
		}
	}
}

func TestAdd_TokenNotInDryRunOutput(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.Execute = false
	results, _ := Add(context.Background(), o)
	for _, r := range results {
		if strings.Contains(r.WouldDo, o.Token) {
			t.Errorf("%s WouldDo expoe token: %q", r.Name, r.WouldDo)
		}
	}
}

func TestRenderTable_DryRun(t *testing.T) {
	t.Parallel()
	o := validOpts()
	results, _ := Add(context.Background(), o)
	var buf bytes.Buffer
	RenderTable(results, o, &buf)
	out := buf.String()
	for _, want := range []string{"DRY-RUN", "peer", "cmpx", "civm", "(seria-aplicado)", "--execute"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderTable omitiu %q", want)
		}
	}
}

func TestRenderTable_Execute(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.Execute = true
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	results, _ := Add(context.Background(), o)
	var buf bytes.Buffer
	RenderTable(results, o, &buf)
	out := buf.String()
	if !strings.Contains(out, "EXECUTE") {
		t.Errorf("output sem EXECUTE")
	}
	if strings.Contains(out, "--execute") {
		t.Errorf("dica --execute apareceu em modo execute")
	}
}

func TestRenderTable_Error(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.Execute = true
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("boom")
	}
	results, _ := Add(context.Background(), o)
	var buf bytes.Buffer
	RenderTable(results, o, &buf)
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("erro nao apareceu na tabela")
	}
}

func TestAdd_InvalidOptions(t *testing.T) {
	t.Parallel()
	o := validOpts()
	o.Repo = ""
	if _, err := Add(context.Background(), o); err == nil {
		t.Errorf("esperava erro de validacao")
	}
}

func TestDefaultOptions_HasSaneDefaults(t *testing.T) {
	t.Parallel()
	d := DefaultOptions()
	if d.Label != "civm" {
		t.Errorf("Label default = %q, want civm", d.Label)
	}
	if d.RunnerVersion == "" {
		t.Errorf("RunnerVersion vazio")
	}
	if d.Execute {
		t.Errorf("Execute default = true; deveria ser false (dry-run)")
	}
}

// ==== Remove tests ====

func validRemoveOpts() RemoveOptions {
	return RemoveOptions{
		Short:          "cmpx",
		Token:          "REMOVE-TOKEN-XYZ",
		BaseDir:        "/home/emdev",
		Execute:        false,
		IdleProbeDelay: 0,
		RunFn:          func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		ActivityFn:     func(context.Context) ([]idle.Activity, error) { return nil, nil },
	}
}

func TestRemove_DryRun_NoExecute(t *testing.T) {
	t.Parallel()
	called := false
	o := validRemoveOpts()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	results, err := Remove(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Errorf("RunFn chamado em dry-run")
	}
	if len(results) != 4 {
		t.Errorf("len(results) = %d, want 4 steps (stop, uninstall, config_remove, remove_dir)", len(results))
	}
	for _, r := range results {
		if r.Executed {
			t.Errorf("%s Executed em dry-run", r.Name)
		}
	}
}

func TestRemove_Execute_RunsAllSteps(t *testing.T) {
	t.Parallel()
	calls := 0
	o := validRemoveOpts()
	o.Execute = true
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	results, err := Remove(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, r := range results {
		if !r.Executed {
			t.Errorf("%s nao Executed", r.Name)
		}
		if r.Err != nil {
			t.Errorf("%s err = %v (steps best-effort, nao deveriam erro)", r.Name, r.Err)
		}
	}
	if calls < 4 {
		t.Errorf("calls = %d, esperava ao menos 4", calls)
	}
}

func TestRemove_TokenNotInWouldDo(t *testing.T) {
	t.Parallel()
	o := validRemoveOpts()
	results, _ := Remove(context.Background(), o)
	for _, r := range results {
		if strings.Contains(r.WouldDo, o.Token) {
			t.Errorf("%s WouldDo expoe token: %q", r.Name, r.WouldDo)
		}
	}
}

func TestRemove_ConfigRemoveErrorIsReported(t *testing.T) {
	t.Parallel()
	o := validRemoveOpts()
	o.Execute = true
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		// config_remove uses `sh -c '… ./config.sh remove …'`
		joined := name + " " + strings.Join(args, " ")
		if strings.Contains(joined, "config.sh remove") {
			return nil, errors.New("config remove falhou")
		}
		return nil, nil
	}
	results, _ := Remove(context.Background(), o)
	var gotConfigErr bool
	for _, r := range results {
		if r.Name == "config_remove" && r.Err != nil {
			gotConfigErr = true
		}
	}
	if !gotConfigErr {
		t.Errorf("config_remove deveria reportar erro")
	}
}

func TestRemove_StopErrorBlocksDestructiveSteps(t *testing.T) {
	t.Parallel()
	o := validRemoveOpts()
	o.Execute = true
	calls := []string{}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		calls = append(calls, key)
		if name == "sudo" && len(args) > 1 && strings.HasSuffix(args[0], "/svc.sh") && args[1] == "stop" {
			return nil, errors.New("stop falhou")
		}
		return nil, nil
	}
	results, _ := Remove(context.Background(), o)
	if len(results) != 1 || results[0].Name != "stop_service" || results[0].Err == nil {
		t.Fatalf("results = %+v, want stop_service error only", results)
	}
	for _, call := range calls {
		if strings.Contains(call, "config.sh") || strings.HasPrefix(call, "rm -rf") {
			t.Fatalf("destructive step ran after stop failure: %v", calls)
		}
	}
}

func TestRemove_ExecuteBlocksWhenHostBusy(t *testing.T) {
	t.Parallel()
	o := validRemoveOpts()
	o.Execute = true
	o.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return []idle.Activity{{PID: 123, Command: "/home/emdev/actions-runner/_work/repo/repo/test.sh"}}, nil
	}
	calls := 0
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	results, err := Remove(context.Background(), o)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(results) != 1 || results[0].Name != "host_idle" {
		t.Fatalf("results = %+v, want host_idle only", results)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "host nao esta ocioso") {
		t.Fatalf("Err = %v, want busy guard", results[0].Err)
	}
	if calls != 0 {
		t.Fatalf("RunFn calls = %d, want 0", calls)
	}
}

func TestValidateRemove_RequiredFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*RemoveOptions)
	}{
		{"no short", func(o *RemoveOptions) { o.Short = "" }},
		{"bad short", func(o *RemoveOptions) { o.Short = "x/y" }},
		{"no base-dir", func(o *RemoveOptions) { o.BaseDir = "" }},
		{"base-dir not clean", func(o *RemoveOptions) { o.BaseDir = "/home/emdev/.." }},
		{"no token", func(o *RemoveOptions) { o.Token = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := validRemoveOpts()
			c.mut(&o)
			if err := validateRemoveOptions(o); err == nil {
				t.Errorf("esperava erro pra %q", c.name)
			}
		})
	}
}

func TestRenderRemoveTable_DryRun(t *testing.T) {
	t.Parallel()
	o := validRemoveOpts()
	results, _ := Remove(context.Background(), o)
	var buf bytes.Buffer
	RenderRemoveTable(results, o, &buf)
	out := buf.String()
	for _, want := range []string{"DRY-RUN", "cmpx", "stop_service", "remove_dir", "--execute"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderRemoveTable omitiu %q", want)
		}
	}
}

func TestRenderRemoveTable_Execute(t *testing.T) {
	t.Parallel()
	o := validRemoveOpts()
	o.Execute = true
	results, _ := Remove(context.Background(), o)
	var buf bytes.Buffer
	RenderRemoveTable(results, o, &buf)
	if !strings.Contains(buf.String(), "EXECUTE") {
		t.Errorf("output sem EXECUTE")
	}
}

func TestRemove_InvalidOpts(t *testing.T) {
	t.Parallel()
	o := validRemoveOpts()
	o.Short = ""
	if _, err := Remove(context.Background(), o); err == nil {
		t.Errorf("esperava erro de validacao")
	}
}

func TestDefaultRemoveOptions(t *testing.T) {
	t.Parallel()
	d := DefaultRemoveOptions()
	if d.Execute {
		t.Errorf("Execute default = true; deveria ser false (dry-run)")
	}
}
