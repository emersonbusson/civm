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

func validUpgradeOpts() UpgradeOptions {
	return UpgradeOptions{
		Short:          "civm-1",
		NewVersion:     "2.335.0",
		RunnerSHA256:   "valid-sha",
		BaseDir:        "/home/emdev",
		Execute:        false,
		VerifyDelay:    0,
		IdleProbeDelay: 0,
		ActivityFn: func(context.Context) ([]idle.Activity, error) {
			return nil, nil
		},
		RunFn: func(_ context.Context, name string, args ...string) ([]byte, error) {
			key := name + " " + strings.Join(args, " ")
			if strings.Contains(key, "list-units") {
				return []byte(fakeListOutput), nil
			}
			return nil, nil
		},
		SHA256FileFn: func(string) (string, error) {
			return "valid-sha", nil
		},
		SleepFn: func(time.Duration) {},
	}
}

func TestUpgrade_DryRun_ResolvesUnitAndDir(t *testing.T) {
	t.Parallel()
	r, err := Upgrade(context.Background(), validUpgradeOpts())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Err != nil {
		t.Fatalf("r.Err = %v", r.Err)
	}
	if !strings.Contains(r.UnitResolved, "civm-1") {
		t.Errorf("UnitResolved = %q", r.UnitResolved)
	}
	if r.Dir != "/home/emdev/actions-runner-civm-1" {
		t.Errorf("Dir = %q", r.Dir)
	}
	if !strings.Contains(r.WouldDo, "2.335.0") {
		t.Errorf("WouldDo sem versao 2.335.0: %q", r.WouldDo)
	}
}

func TestUpgrade_Execute_HappyPath(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Execute = true
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
	r, _ := Upgrade(context.Background(), o)
	if r.Err != nil {
		t.Errorf("r.Err = %v", r.Err)
	}
	for _, want := range []bool{r.StoppedOK, r.DownloadedOK, r.ExtractedOK, r.StartedOK, r.ActiveAfter} {
		if !want {
			t.Errorf("nem todos steps OK: stopped=%v dl=%v ext=%v start=%v active=%v",
				r.StoppedOK, r.DownloadedOK, r.ExtractedOK, r.StartedOK, r.ActiveAfter)
			break
		}
	}
	hasStop, hasCurl, hasTar, hasStart := false, false, false, false
	for _, c := range calls {
		if strings.Contains(c, "systemctl stop") {
			hasStop = true
		}
		if strings.HasPrefix(c, "curl") {
			hasCurl = true
		}
		if strings.HasPrefix(c, "tar") {
			hasTar = true
		}
		if strings.Contains(c, "systemctl start") {
			hasStart = true
		}
	}
	if !hasStop || !hasCurl || !hasTar || !hasStart {
		t.Errorf("seq incompleta: stop=%v curl=%v tar=%v start=%v", hasStop, hasCurl, hasTar, hasStart)
	}
}

func TestUpgrade_DownloadFails_RollbackStart(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Execute = true
	calls := []string{}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		calls = append(calls, key)
		if strings.Contains(key, "list-units") {
			return []byte(fakeListOutput), nil
		}
		if strings.HasPrefix(key, "curl") {
			return nil, errors.New("404 not found")
		}
		return nil, nil
	}
	r, _ := Upgrade(context.Background(), o)
	if r.Err == nil {
		t.Errorf("esperava erro de download")
	}
	if !r.StoppedOK {
		t.Errorf("StoppedOK false")
	}
	if r.DownloadedOK {
		t.Errorf("DownloadedOK true mesmo com erro")
	}
	// Verifica rollback start (tentar reiniciar runner mesmo após falha)
	startCount := 0
	for _, c := range calls {
		if strings.Contains(c, "systemctl start") {
			startCount++
		}
	}
	if startCount < 1 {
		t.Errorf("rollback start nao chamado")
	}
}

func TestUpgrade_VerifiesRunnerSHA256BeforeExtract(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Execute = true
	o.SHA256FileFn = func(string) (string, error) {
		return "bad-sha", nil
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
	r, _ := Upgrade(context.Background(), o)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "sha256") {
		t.Fatalf("Err = %v, want sha256 mismatch", r.Err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "tar ") {
			t.Fatalf("tar ran after checksum failure: %v", calls)
		}
	}
}

func TestUpgrade_StopFails(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Execute = true
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if strings.Contains(key, "list-units") {
			return []byte(fakeListOutput), nil
		}
		if strings.Contains(key, "systemctl stop") {
			return nil, errors.New("permission denied")
		}
		return nil, nil
	}
	r, _ := Upgrade(context.Background(), o)
	if r.Err == nil {
		t.Errorf("esperava erro de stop")
	}
	if r.StoppedOK {
		t.Errorf("StoppedOK true mesmo com erro")
	}
}

func TestUpgrade_ExecuteBlocksWhenHostBusy(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Execute = true
	o.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return []idle.Activity{{PID: 44, Command: "docker build ."}}, nil
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
	r, _ := Upgrade(context.Background(), o)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "host nao esta ocioso") {
		t.Fatalf("Err = %v, want busy guard", r.Err)
	}
	for _, c := range calls {
		if strings.Contains(c, "systemctl stop") {
			t.Fatalf("systemctl stop called despite busy host: %v", calls)
		}
	}
}

func TestUpgrade_ExtractFails_RollbackStart(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Execute = true
	calls := []string{}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		calls = append(calls, key)
		if strings.Contains(key, "list-units") {
			return []byte(fakeListOutput), nil
		}
		if strings.HasPrefix(key, "tar") {
			return nil, errors.New("disk full")
		}
		return nil, nil
	}
	r, _ := Upgrade(context.Background(), o)
	if r.Err == nil {
		t.Errorf("esperava erro de extract")
	}
	startCount := 0
	for _, c := range calls {
		if strings.Contains(c, "systemctl start") {
			startCount++
		}
	}
	if startCount < 1 {
		t.Errorf("rollback start nao chamado apos extract fail")
	}
}

func TestUpgrade_NotActiveAfter(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Execute = true
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if strings.Contains(key, "list-units") {
			return []byte(fakeListOutput), nil
		}
		if strings.Contains(key, "is-active") {
			return []byte("activating\n"), nil
		}
		return nil, nil
	}
	r, _ := Upgrade(context.Background(), o)
	if r.Err == nil {
		t.Errorf("esperava erro porque nao voltou active")
	}
	if r.ActiveAfter {
		t.Errorf("ActiveAfter true com is-active=activating")
	}
}

func TestValidateUpgrade_Required(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*UpgradeOptions)
	}{
		{"no short/unit", func(o *UpgradeOptions) { o.Short = ""; o.Unit = "" }},
		{"no new-version", func(o *UpgradeOptions) { o.NewVersion = "" }},
		{"bad new-version", func(o *UpgradeOptions) { o.NewVersion = "2.335" }},
		{"bad short", func(o *UpgradeOptions) { o.Short = "../x" }},
		{"bad unit", func(o *UpgradeOptions) { o.Short = ""; o.Unit = "../x.service" }},
		{"no base-dir", func(o *UpgradeOptions) { o.BaseDir = "" }},
		{"base-dir not clean", func(o *UpgradeOptions) { o.BaseDir = "/home/emdev/.." }},
		{"dir not clean", func(o *UpgradeOptions) { o.Dir = "/home/emdev/../runner" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := validUpgradeOpts()
			c.mut(&o)
			if err := validateUpgradeOptions(o); err == nil {
				t.Errorf("esperava erro pra %q", c.name)
			}
		})
	}
}

func TestValidateUpgrade_UnitOrShortAccepted(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Short = ""
	o.Unit = "actions.runner.foo.service"
	if err := validateUpgradeOptions(o); err != nil {
		t.Errorf("Unit-only deveria passar: %v", err)
	}
}

func TestUpgrade_DefaultUpgradeOptions(t *testing.T) {
	t.Parallel()
	d := DefaultUpgradeOptions()
	if d.Execute {
		t.Errorf("Execute default = true; deveria ser false")
	}
	if d.VerifyDelay != 5*time.Second {
		t.Errorf("VerifyDelay default = %v, want 5s", d.VerifyDelay)
	}
}

func TestRenderUpgradeTable_DryRun(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	r, _ := Upgrade(context.Background(), o)
	var buf bytes.Buffer
	RenderUpgradeTable(r, o, &buf)
	out := buf.String()
	for _, want := range []string{"DRY-RUN", "2.335.0", "civm-1", "(seria-aplicado)", "--execute"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderUpgrade omitiu %q", want)
		}
	}
}

func TestRenderUpgradeTable_ExecuteSuccess(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Execute = true
	r := UpgradeResult{
		UnitResolved: "x.service", Dir: "/y", NewVersion: "2.335.0",
		StoppedOK: true, DownloadedOK: true, ExtractedOK: true,
		StartedOK: true, ActiveAfter: true,
	}
	var buf bytes.Buffer
	RenderUpgradeTable(r, o, &buf)
	out := buf.String()
	if !strings.Contains(out, "EXECUTE") {
		t.Errorf("output sem EXECUTE")
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("output sem mensagem OK")
	}
}

func TestGuessDirFromShort(t *testing.T) {
	t.Parallel()
	if got := guessDirFromShort("/home/emdev", "cmpx"); got != "/home/emdev/actions-runner-cmpx" {
		t.Errorf("got %q", got)
	}
	if got := guessDirFromShort("/x", ""); got != "/x" {
		t.Errorf("empty short = %q", got)
	}
}

func TestUpgrade_ExplicitUnit(t *testing.T) {
	t.Parallel()
	o := validUpgradeOpts()
	o.Short = ""
	o.Unit = "actions.runner.x-y.foo.service"
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	r, _ := Upgrade(context.Background(), o)
	if r.UnitResolved != o.Unit {
		t.Errorf("UnitResolved = %q, want %q", r.UnitResolved, o.Unit)
	}
}
