package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/specs"
)

type runCall struct {
	name string
	args []string
}

func newRunner(responses map[string][]byte, errs map[string]error) (func(context.Context, string, ...string) ([]byte, error), *[]runCall) {
	calls := []runCall{}
	fn := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, runCall{name, args})
		key := name
		if len(args) > 0 {
			key = name + " " + args[0]
		}
		if err, ok := errs[key]; ok {
			return nil, err
		}
		if r, ok := responses[key]; ok {
			return r, nil
		}
		if r, ok := responses[name]; ok {
			return r, nil
		}
		return nil, errors.New("nao mockado: " + key)
	}
	return fn, &calls
}

func okOpts(t *testing.T) Options {
	t.Helper()
	opts := DefaultOptions()
	opts.UID = 0
	opts.WriteFileFn = func(string, []byte, fs.FileMode) error { return nil }
	opts.OSReader = func() (string, error) {
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\n", nil
	}
	return opts
}

func TestRun_DryRun_Reports_Would_Apply(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.Execute = false
	fn, _ := newRunner(map[string][]byte{}, map[string]error{
		"go --version":         errors.New("not installed"),
		"node --version":       errors.New("not installed"),
		"docker --version":     errors.New("not installed"),
		"gh --version":         errors.New("not installed"),
		"dpkg-query":           errors.New("not installed"),
		"systemctl is-enabled": errors.New("disabled"),
	})
	opts.RunFn = fn
	results := Run(context.Background(), opts)
	if len(results) == 0 {
		t.Fatalf("0 results")
	}
	wouldDoCount := 0
	for _, r := range results {
		if r.WouldDo {
			wouldDoCount++
		}
		if r.Executed {
			t.Errorf("%s Executed em dry-run", r.Name)
		}
	}
	if wouldDoCount == 0 {
		t.Errorf("nenhum step com WouldDo=true em dry-run")
	}
}

func TestRun_VerifyOSFails(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.OSReader = func() (string, error) {
		return "ID=debian\nVERSION_ID=\"12\"\n", nil
	}
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("nao deveria ser chamado")
	}
	results := Run(context.Background(), opts)
	if results[0].Err == nil {
		t.Errorf("verify_os deveria ter erro com Debian")
	}
}

func TestRun_ExecuteStopsAfterPreflightError(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.Execute = true
	opts.OSReader = func() (string, error) {
		return "ID=debian\nVERSION_ID=\"12\"\n", nil
	}
	calls := 0
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("nao deveria executar comandos apos preflight")
	}
	results := Run(context.Background(), opts)
	if len(results) != 1 || results[0].Name != "verify_os" || results[0].Err == nil {
		t.Fatalf("results = %+v, want only verify_os error", results)
	}
	if calls != 0 {
		t.Fatalf("RunFn calls = %d, want 0", calls)
	}
}

func TestRun_NotRoot(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.UID = 1000
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("nao deveria ser chamado")
	}
	results := Run(context.Background(), opts)
	foundUIDError := false
	for _, r := range results {
		if r.Name == "verify_uid" && r.Err != nil {
			foundUIDError = true
		}
	}
	if !foundUIDError {
		t.Errorf("UID 1000 deveria gerar erro em verify_uid")
	}
}

func TestInstallGoTarball_VerifiesSHA256BeforeExtract(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.SHA256FileFn = func(string) (string, error) {
		return "bad-sha", nil
	}
	calls := []string{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	err := installGoTarball(context.Background(), opts, "1.26.3")
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "tar ") || strings.HasPrefix(call, "rm -rf /usr/local/go") {
			t.Fatalf("mutating command ran before checksum success: %v", calls)
		}
	}
}

func TestInstallGoTarball_SuccessAfterSHA256(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.SHA256FileFn = func(string) (string, error) {
		return "2b2cfc7148493da5e73981bffbf3353af381d5f93e789c82c79aff64962eb556", nil
	}
	calls := []string{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := installGoTarball(context.Background(), opts, "1.26.3"); err != nil {
		t.Fatalf("installGoTarball err = %v", err)
	}
	assertCommandRan(t, calls, "tar -C /usr/local -xzf /tmp/go-1.26.3.tar.gz")
	assertCommandRan(t, calls, "ln -sf /usr/local/go/bin/go /usr/local/bin/go")
}

func TestInstallNodeViaNodeSource_VerifiesSHA256BeforeBash(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.SHA256FileFn = func(string) (string, error) {
		return "bad-sha", nil
	}
	calls := []string{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	err := installNodeViaNodeSource(context.Background(), opts, "24.15.0")
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "bash ") || strings.HasPrefix(call, "apt-get install") {
			t.Fatalf("command ran before checksum success: %v", calls)
		}
	}
}

func TestInstallNodeViaNodeSource_SuccessAfterSHA256(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.SHA256FileFn = func(string) (string, error) {
		return "6e3d580f5bd7ccf2aa1e8df8d35c60d78e873c3ff8beb282c9bebd914904ad72", nil
	}
	calls := []string{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := installNodeViaNodeSource(context.Background(), opts, "24.15.0"); err != nil {
		t.Fatalf("installNodeViaNodeSource err = %v", err)
	}
	assertCommandRan(t, calls, "bash /tmp/nodesource-24.sh")
	assertCommandRan(t, calls, "apt-get install -y nodejs")
}

func TestInstallYQBinary_VerifiesSHA256BeforeInstall(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.SHA256FileFn = func(string) (string, error) {
		return "bad-sha", nil
	}
	calls := []string{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	err := installYQBinary(context.Background(), opts, "4.52.5")
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "install ") {
			t.Fatalf("install ran before checksum success: %v", calls)
		}
	}
}

func TestInstallYQBinary_SuccessAfterSHA256(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.SHA256FileFn = func(string) (string, error) {
		return "75d893a0d5940d1019cb7cdc60001d9e876623852c31cfc6267047bc31149fa9", nil
	}
	calls := []string{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := installYQBinary(context.Background(), opts, "4.52.5"); err != nil {
		t.Fatalf("installYQBinary err = %v", err)
	}
	assertCommandRan(t, calls, "install -m 0755 /tmp/yq-4.52.5-linux-amd64 /usr/local/bin/yq")
}

func TestRun_Execute_AlreadyInstalled_Skips(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.Execute = true
	want := opts.Spec.Tools[0].Preferred() // go
	wantNode := "v" + opts.Spec.Tools[1].Preferred()
	wantDocker := opts.Spec.Tools[3].Preferred()
	wantGH := opts.Spec.Tools[5].Preferred()
	wantYQ := opts.Spec.Tools[8].Preferred()
	responses := map[string][]byte{
		"go version":       []byte("go version go" + want + " linux/amd64\n"),
		"node --version":   []byte(wantNode + "\n"),
		"docker --version": []byte("Docker version " + wantDocker + ", build abc\n"),
		"gh --version":     []byte("gh version " + wantGH + " (2026)\n"),
		"yq --version":     []byte("yq (https://github.com/mikefarah/yq/) version v" + wantYQ + "\n"),
		"dpkg-query": []byte("build-essential install ok installed\n" +
			"curl install ok installed\n" +
			"wget install ok installed\n" +
			"jq install ok installed\n" +
			"git install ok installed\n" +
			"python3 install ok installed\n" +
			"python3-pip install ok installed\n" +
			"python3-venv install ok installed\n" +
			"ca-certificates install ok installed\n"),
		"systemctl is-enabled": []byte("enabled\n"),
	}
	fn, calls := newRunner(responses, nil)
	opts.RunFn = fn
	results := Run(context.Background(), opts)
	for _, r := range results {
		if r.Name == "verify_os" || r.Name == "verify_uid" {
			continue
		}
		if !r.AlreadyDone {
			t.Errorf("%s deveria estar AlreadyDone", r.Name)
		}
		if r.Executed {
			t.Errorf("%s Executed = true mesmo com AlreadyDone", r.Name)
		}
	}
	for _, c := range *calls {
		if c.name == "apt-get" && len(c.args) > 0 && c.args[0] == "install" {
			t.Errorf("apt-get install chamado mesmo com tudo ja instalado")
		}
	}
}

func TestRenderTable_Snapshot(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Execute = false
	results := []Result{
		{Name: "verify_os", Description: "x", AlreadyDone: true},
		{Name: "install_go", Description: "Instala Go 1.25.9", WouldDo: true},
		{Name: "broken", Description: "y", Err: errors.New("falha")},
	}
	var buf bytes.Buffer
	RenderTable(results, opts, &buf)
	out := buf.String()
	for _, want := range []string{"DRY-RUN", "ja-instalado", "(seria-aplicado)", "erro:", "Resumo:"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderTable omitiu %q\noutput:\n%s", want, out)
		}
	}
}

func TestRenderTable_Execute(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Execute = true
	results := []Result{{Name: "x", Description: "y", Executed: true}}
	var buf bytes.Buffer
	RenderTable(results, opts, &buf)
	if !strings.Contains(buf.String(), "EXECUTE") {
		t.Errorf("output sem EXECUTE")
	}
}

func TestSpecHasGoNodeDocker(t *testing.T) {
	t.Parallel()
	s := specs.Ubuntu2404()
	for _, name := range []string{"go", "node", "docker", "gh"} {
		if _, ok := s.FindTool(name); !ok {
			t.Errorf("spec sem %s", name)
		}
	}
}

func TestFirstLine(t *testing.T) {
	t.Parallel()
	if got := firstLine("a\nb"); got != "a" {
		t.Errorf("got %q, want a", got)
	}
	if got := firstLine("noNewline"); got != "noNewline" {
		t.Errorf("got %q, want noNewline", got)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("got %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "abcd…" {
		t.Errorf("got %q", got)
	}
}

func TestRun_WatchdogTimer_OnlyCleanup(t *testing.T) {
	t.Parallel()
	// WatchdogTimer=false + ReverseWatchdog=false: só checa cleanup
	opts := okOpts(t)
	opts.WatchdogTimer = false
	opts.ReverseWatchdog = false
	opts.RunnerWatchdog = false
	opts.MetricsTimer = false
	opts.RunReaper = false
	opts.MemWatchdog = false
	opts.Execute = false
	calls := []string{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name
		if len(args) > 0 {
			key = name + " " + strings.Join(args, " ")
		}
		calls = append(calls, key)
		if strings.Contains(key, "is-enabled civmctl-cleanup.timer") ||
			strings.Contains(key, "is-enabled civmctl-buildcache-prune.timer") {
			return []byte("enabled\n"), nil
		}
		// outros: erro
		return nil, errors.New("not installed")
	}
	results := Run(context.Background(), opts)
	for _, r := range results {
		if r.Name == "install_systemd_timers" {
			if !r.AlreadyDone {
				t.Errorf("WatchdogTimer=false + cleanup enabled: AlreadyDone esperado true")
			}
		}
	}
	for _, c := range calls {
		if strings.Contains(c, "civmctl-disk-watchdog.timer") {
			t.Errorf("WatchdogTimer=false NAO deveria chamar disk-watchdog.timer; got: %q", c)
		}
		if strings.Contains(c, "civmctl-runner-watchdog.timer") {
			t.Errorf("RunnerWatchdog=false NAO deveria chamar runner-watchdog.timer; got: %q", c)
		}
		if strings.Contains(c, "civmctl-metrics.timer") {
			t.Errorf("MetricsTimer=false NAO deveria chamar metrics.timer; got: %q", c)
		}
	}
}

func TestRun_WatchdogTimer_BothRequired(t *testing.T) {
	t.Parallel()
	// WatchdogTimer=true: ambos timers checados
	opts := okOpts(t)
	opts.WatchdogTimer = true
	opts.RunnerWatchdog = true
	opts.Execute = false
	calls := []string{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name
		if len(args) > 0 {
			key = name + " " + strings.Join(args, " ")
		}
		calls = append(calls, key)
		// Cleanup enabled mas watchdog não
		if strings.Contains(key, "is-enabled civmctl-cleanup.timer") {
			return []byte("enabled\n"), nil
		}
		return nil, errors.New("not installed")
	}
	results := Run(context.Background(), opts)
	for _, r := range results {
		if r.Name == "install_systemd_timers" {
			if r.AlreadyDone {
				t.Errorf("watchdog ausente: AlreadyDone deveria ser false")
			}
			if !r.WouldDo {
				t.Errorf("watchdog ausente em dry-run: WouldDo esperado true")
			}
		}
	}
}

func TestTimerListIncludesRunnerWatchdogByDefault(t *testing.T) {
	t.Parallel()
	timers := strings.Join(timerList(DefaultOptions()), ",")
	if !strings.Contains(timers, "civmctl-runner-watchdog.timer") {
		t.Fatalf("timerList default = %s, want runner-watchdog", timers)
	}
	if !strings.Contains(timers, "civmctl-metrics.timer") {
		t.Fatalf("timerList default = %s, want metrics", timers)
	}
	if !strings.Contains(timers, "civmctl-run-reaper.timer") {
		t.Fatalf("timerList default = %s, want run-reaper", timers)
	}
	if !strings.Contains(timers, "civmctl-mem-watchdog.timer") {
		t.Fatalf("timerList default = %s, want mem-watchdog", timers)
	}
	opts := DefaultOptions()
	opts.RunnerWatchdog = false
	opts.MetricsTimer = false
	opts.RunReaper = false
	opts.MemWatchdog = false
	timers = strings.Join(timerList(opts), ",")
	if strings.Contains(timers, "civmctl-runner-watchdog.timer") {
		t.Fatalf("RunnerWatchdog=false still included runner-watchdog: %s", timers)
	}
	if strings.Contains(timers, "civmctl-metrics.timer") {
		t.Fatalf("MetricsTimer=false still included metrics: %s", timers)
	}
	if strings.Contains(timers, "civmctl-run-reaper.timer") {
		t.Fatalf("RunReaper=false still included run-reaper: %s", timers)
	}
	if strings.Contains(timers, "civmctl-mem-watchdog.timer") {
		t.Fatalf("MemWatchdog=false still included mem-watchdog: %s", timers)
	}
}

func TestRun_StepError_Propagates(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.Execute = true
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "go" || name == "node" || name == "docker" || name == "gh" || name == "dpkg-query" || name == "systemctl" {
			return nil, errors.New("not installed")
		}
		if name == "apt-get" && len(args) > 0 && args[0] == "update" {
			return nil, errors.New("apt-get update falhou")
		}
		return nil, nil
	}
	results := Run(context.Background(), opts)
	hasError := false
	for _, r := range results {
		if r.Err != nil {
			hasError = true
		}
	}
	if !hasError {
		t.Errorf("esperava propagar erro de apt-get update")
	}
}

func TestCopySystemdUnitsCopiesMatchedFilesWithoutShell(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"civmctl-cleanup.service", "civmctl-cleanup.timer"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("unit"), 0644); err != nil { //nolint:gosec // G306: arquivo systemd unit em t.TempDir, replica perm real de /etc/systemd/system
			t.Fatal(err)
		}
	}
	opts := okOpts(t)
	opts.InstallUnitsFrom = dir
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := copySystemdUnits(context.Background(), opts); err != nil {
		t.Fatalf("copySystemdUnits err = %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "sh ") {
			t.Fatalf("copySystemdUnits usou shell: %s", call)
		}
		if !strings.Contains(call, "cp ") || !strings.Contains(call, "/etc/systemd/system/") {
			t.Fatalf("call inesperada: %s", call)
		}
	}
}

func TestCopySystemdUnitsErrorsWhenNoUnits(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.InstallUnitsFrom = t.TempDir()
	if err := copySystemdUnits(context.Background(), opts); err == nil {
		t.Fatalf("esperava erro sem units")
	}
}

func TestCopyDeployScriptsInstallsBinScripts(t *testing.T) {
	t.Parallel()
	// Estrutura real: deploy/systemd (units) + deploy/bin (scripts irmaos). O
	// copyDeployScripts deve instalar os .sh de ../bin em /usr/local/bin com +x,
	// senao a unit do prune fica com ConditionPathExists faltante (no-op).
	root := t.TempDir()
	sysDir := filepath.Join(root, "systemd")
	binDir := filepath.Join(root, "bin")
	for _, d := range []string{sysDir, binDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "civm-ci-artifact-prune.sh"), []byte("#!/usr/bin/env bash\n"), 0o755); err != nil { //nolint:gosec // script em t.TempDir, replica perm real de /usr/local/bin
		t.Fatal(err)
	}
	opts := okOpts(t)
	opts.InstallUnitsFrom = sysDir
	var calls []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := copyDeployScripts(context.Background(), opts); err != nil {
		t.Fatalf("copyDeployScripts err = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1: %v", len(calls), calls)
	}
	call := calls[0]
	if strings.HasPrefix(call, "sh ") {
		t.Fatalf("copyDeployScripts usou shell: %s", call)
	}
	if !strings.Contains(call, "install -m 0755") || !strings.Contains(call, "/usr/local/bin/civm-ci-artifact-prune.sh") {
		t.Fatalf("call inesperada: %s", call)
	}
}

func TestCopyDeployScriptsTolerantWhenNoBinDir(t *testing.T) {
	t.Parallel()
	// InstallUnitsFrom sem um ../bin irmao: nada a instalar, sem erro (o bootstrap
	// nao deve falhar so porque um deploy custom nao traz scripts).
	root := t.TempDir()
	sysDir := filepath.Join(root, "systemd")
	if err := os.MkdirAll(sysDir, 0o750); err != nil {
		t.Fatal(err)
	}
	opts := okOpts(t)
	opts.InstallUnitsFrom = sysDir
	var calls int
	opts.RunFn = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	if err := copyDeployScripts(context.Background(), opts); err != nil {
		t.Fatalf("copyDeployScripts err = %v", err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0 (nada a instalar)", calls)
	}
}

func TestInstallDockerCEWritesSourceWithoutShell(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.OSReader = func() (string, error) {
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\n", nil
	}
	var commands []string
	var writtenPath string
	var writtenData string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		switch name {
		case "dpkg":
			return []byte("amd64\n"), nil
		case "gpg":
			return []byte("fpr:::::::::" + "9DC858229FC7DD38854AE2D88D81803C0EBFCD88" + ":\n"), nil
		default:
			return nil, nil
		}
	}
	opts.WriteFileFn = func(path string, data []byte, perm fs.FileMode) error {
		writtenPath = path
		writtenData = string(data)
		return nil
	}
	if err := installDockerCE(context.Background(), opts); err != nil {
		t.Fatalf("installDockerCE err = %v", err)
	}
	if writtenPath != "/etc/apt/sources.list.d/docker.list" {
		t.Fatalf("writtenPath = %q", writtenPath)
	}
	for _, want := range []string{"arch=amd64", " noble ", "download.docker.com"} {
		if !strings.Contains(writtenData, want) {
			t.Fatalf("docker source sem %q: %s", want, writtenData)
		}
	}
	for _, command := range commands {
		if strings.HasPrefix(command, "sh ") {
			t.Fatalf("installDockerCE usou shell: %s", command)
		}
		if strings.HasPrefix(command, "lsb_release ") {
			t.Fatalf("installDockerCE nao deveria depender de lsb_release: %s", command)
		}
	}
}

func TestInstallDockerCEBlocksBadKeyFingerprint(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.OSReader = func() (string, error) {
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\n", nil
	}
	var commands []string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		switch name {
		case "dpkg":
			return []byte("amd64\n"), nil
		case "gpg":
			return []byte("fpr:::::::::BAD:\n"), nil
		default:
			return nil, nil
		}
	}
	err := installDockerCE(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("err = %v, want fingerprint mismatch", err)
	}
	for _, command := range commands {
		if strings.HasPrefix(command, "apt-get ") {
			t.Fatalf("apt-get ran after bad fingerprint: %v", commands)
		}
	}
}

func TestUbuntuCodenameFallbackForUbuntu2404(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.OSReader = func() (string, error) {
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\n", nil
	}
	got, err := ubuntuCodename(opts)
	if err != nil {
		t.Fatalf("ubuntuCodename err = %v", err)
	}
	if got != "noble" {
		t.Fatalf("ubuntuCodename = %q, want noble", got)
	}
}

func TestInstallGHCLIWritesSourceWithoutShell(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	var commands []string
	var writtenData string
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "dpkg" {
			return []byte("arm64\n"), nil
		}
		if name == "gpg" {
			return []byte("fpr:::::::::" + "2C6106201985B60E6C7AC87323F3D4EA75716059" + ":\n"), nil
		}
		return nil, nil
	}
	opts.WriteFileFn = func(path string, data []byte, perm fs.FileMode) error {
		if path != "/etc/apt/sources.list.d/github-cli.list" {
			t.Fatalf("path = %q", path)
		}
		writtenData = string(data)
		return nil
	}
	if err := installGHCLI(context.Background(), opts); err != nil {
		t.Fatalf("installGHCLI err = %v", err)
	}
	if !strings.Contains(writtenData, "arch=arm64") {
		t.Fatalf("github source sem arch=arm64: %s", writtenData)
	}
	for _, command := range commands {
		if strings.HasPrefix(command, "sh ") {
			t.Fatalf("installGHCLI usou shell: %s", command)
		}
	}
}

func TestCommandOutputTrimmedRejectsEmpty(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(" \n"), nil
	}
	if _, err := commandOutputTrimmed(context.Background(), opts, "dpkg", "--print-architecture"); err == nil {
		t.Fatalf("esperava erro para output vazio")
	}
}

func assertCommandRan(t *testing.T, calls []string, want string) {
	t.Helper()
	for _, call := range calls {
		if call == want {
			return
		}
	}
	t.Fatalf("command %q not found in calls: %v", want, calls)
}
