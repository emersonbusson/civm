package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/hook"
)

// writeHealthyHostMetrics writes a fresh, healthy host-metrics snapshot so the
// host-aware gate never forces cleanup during a dispatch test. Without it the
// test reads the REAL delivered snapshot (or its absence) and the asserted
// decision flips with the live host V: pressure — an environmental assumption,
// not what these tests prove (argv[0] dispatch).
func writeHealthyHostMetrics(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "host-metrics.json")
	snapshot := fmt.Sprintf(
		`{"v_free_gb":100,"v_size_gb":200,"vhdx_file_size_gb":50,"vhdx_min_size_gb":50,"vhdx_max_size_gb":110,"guest_free_gb":80,"gap_gb":1,"vm_state":"Running","timestamp":%q}`,
		time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(snapshot), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// civmctlBin is the path to a built civmctl binary, set up once per
// package by TestMain. Integration tests use this binary plus symlinks
// to exercise argv[0]-based hook dispatch end to end.
var civmctlBin string

type integrationEvent struct {
	Event string `json:"event"`
}

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "civmctl-int-*")
	if err != nil {
		panic(err)
	}
	civmctlBin = filepath.Join(tmp, "civmctl")
	build := exec.Command("go", "build", "-o", civmctlBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		panic("build civmctl: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// TestIntegration_JobStartedSymlinkDispatch valida que invocar civmctl via
// symlink "job-started" no /opt/civm/hooks roteia para o subcomando de
// hook com --execute, retornando JSON estruturado em vez de mensagem de
// help genérica. Thresholds altos garantem que cleanup não dispara.
func TestIntegration_JobStartedSymlinkDispatch(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "job-started")
	if err := os.Symlink(civmctlBin, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link,
		"--pre-cleanup-pct=99",
		"--hard-fail-pct=100",
		"--min-free-gb=0",
		"--host-metrics-path="+writeHealthyHostMetrics(t, dir),
		"--json",
	)
	cmd.Env = []string{
		"HOME=" + dir,
		"PATH=/usr/bin:/bin",
		// RUNNER_TEMP sob tmpdir falha safeWorkRoot → cleanup não enumera roots reais.
		"RUNNER_TEMP=" + filepath.Join(dir, "fake-runner-temp"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("symlink invocation failed: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if res["event"] != "job-started" {
		t.Errorf("event=%v, want job-started\n%s", res["event"], out)
	}
	if res["decision"] != "ok" {
		t.Errorf("decision=%v, want ok\n%s", res["decision"], out)
	}
}

// TestIntegration_LegacyShSymlinkDispatch valida o caminho de compat com
// instalações antigas que usavam wrappers .sh — civmctl tolera o sufixo
// no basename para não quebrar runners ainda não re-instalados.
func TestIntegration_LegacyShSymlinkDispatch(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "job-started.sh")
	if err := os.Symlink(civmctlBin, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link,
		"--pre-cleanup-pct=99",
		"--hard-fail-pct=100",
		"--min-free-gb=0",
		"--json",
	)
	cmd.Env = []string{
		"HOME=" + dir,
		"PATH=/usr/bin:/bin",
		"RUNNER_TEMP=" + filepath.Join(dir, "fake-runner-temp"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("legacy .sh symlink failed: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if res["event"] != "job-started" {
		t.Errorf("legacy .sh basename did not dispatch: event=%v\n%s", res["event"], out)
	}
}

func TestIntegration_ManagedHookScriptDispatchViaBash(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "job-started.sh")
	if err := os.WriteFile(script, []byte(hook.ScriptContent(civmctlBin, hook.EventJobStarted)), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script,
		"--pre-cleanup-pct=99",
		"--hard-fail-pct=100",
		"--min-free-gb=0",
		"--host-metrics-path="+writeHealthyHostMetrics(t, dir),
		"--json",
	)
	cmd.Env = []string{
		"HOME=" + dir,
		"PATH=/usr/bin:/bin",
		"RUNNER_TEMP=" + filepath.Join(dir, "fake-runner-temp"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("managed hook script failed: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if res["event"] != "job-started" {
		t.Errorf("managed script did not dispatch: event=%v\n%s", res["event"], out)
	}
	if res["decision"] != "ok" {
		t.Errorf("decision=%v, want ok\n%s", res["decision"], out)
	}
}

// TestIntegration_NoSymlinkRunsAsRegularCLI valida que invocações normais
// (não via symlink) seguem o switch padrão e NÃO caem no dispatch de hook.
// Sem isso, qualquer rename acidental do binário viraria modo hook.
func TestIntegration_NoSymlinkRunsAsRegularCLI(t *testing.T) {
	cmd := exec.Command(civmctlBin, "version-pins")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("version-pins failed: %v\n%s", err, out)
	}
	// version-pins emite versões alvo Ubuntu 2404; deve mencionar Go.
	if !strings.Contains(string(out), "Go") && !strings.Contains(string(out), "go") {
		t.Errorf("version-pins output sem menção a Go:\n%s", out)
	}
}

// TestIntegration_UnrelatedBasenameDoesNotDispatch valida que basenames
// fora da whitelist (job-started/job-completed) NÃO ativam modo hook.
// Importante: o binário renomeado para "civmctl-foo" deve mostrar help.
func TestIntegration_UnrelatedBasenameDoesNotDispatch(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "civmctl-foo")
	if err := os.Symlink(civmctlBin, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link)
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
	out, _ := cmd.CombinedOutput()
	// Sem args, civmctl mostra help e sai com exitUsage. Não deve mostrar JSON.
	if strings.Contains(string(out), `"event"`) {
		t.Errorf("non-whitelisted basename triggered hook dispatch:\n%s", out)
	}
	if !strings.Contains(string(out), "civmctl — provisionamento") {
		t.Errorf("expected help message, got:\n%s", out)
	}
}

func TestIntegration_RunnerWatchdogExecuteFakeRestartsAndReruns(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil { //nolint:gosec // G301: temp PATH dir needs executable bit.
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "commands.log")
	runnerDir := filepath.Join(dir, "actions-runner-self")
	if err := os.MkdirAll(runnerDir, 0755); err != nil { //nolint:gosec // G301: fake runner dir needs traversal bit.
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runnerDir, ".runner"), []byte(`{"gitHubUrl":"https://github.com/acme/civm"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runnerDir, ".env"), []byte("EXISTING=1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	writeFakeCommand(t, binDir, "git", `#!/usr/bin/env bash
printf 'git %s\n' "$*" >> "$CIVM_FAKE_LOG"
exit 0
`)
	writeFakeCommand(t, binDir, "ps", `#!/usr/bin/env bash
printf 'ps %s\n' "$*" >> "$CIVM_FAKE_LOG"
printf '1 0 init /sbin/init\n'
`)
	writeFakeCommand(t, binDir, "systemctl", `#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >> "$CIVM_FAKE_LOG"
if [ "$1" = "list-units" ]; then
  printf 'actions.runner.acme-civm.civm-self.service loaded failed failed GitHub Actions Runner\n'
  exit 0
fi
if [ "$1" = "show" ]; then
  printf '%s\n' "$CIVM_FAKE_RUNNER_DIR"
  exit 0
fi
if [ "$1" = "is-active" ]; then
  printf 'active\n'
  exit 0
fi
exit 0
`)
	writeFakeCommand(t, binDir, "sudo", `#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >> "$CIVM_FAKE_LOG"
exit 0
`)
	writeFakeCommand(t, binDir, "gh", `#!/usr/bin/env bash
printf 'gh %s\n' "$*" >> "$CIVM_FAKE_LOG"
if [ "$1" = "api" ] && [ "$2" = "/repos/acme/civm/actions/runners" ]; then
  cat <<'JSON'
{"runners":[{"id":1,"name":"civm-self","status":"online","busy":false,"labels":[{"name":"self-hosted"},{"name":"civm"}]}]}
JSON
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "/repos/acme/civm/actions/runs?per_page=20&status=completed" ]; then
  created_at="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  cat <<JSON
{"workflow_runs":[{"id":99,"head_sha":"abc123","status":"completed","conclusion":"failure","created_at":"$created_at","html_url":"https://github.com/acme/civm/actions/runs/99","pull_requests":[{"number":7}]}]}
JSON
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "/repos/acme/civm/pulls/7" ]; then
  printf '{"number":7,"state":"open","mergeable_state":"clean"}\n'
  exit 0
fi
if [ "$1" = "run" ] && [ "$2" = "view" ] && [ "$3" = "99" ]; then
  printf 'Run actions/checkout@v5\nRPC failed; curl 56 GnuTLS recv error\nfatal: early EOF\n'
  exit 0
fi
if [ "$1" = "run" ] && [ "$2" = "rerun" ] && [ "$3" = "99" ]; then
  exit 0
fi
printf 'unexpected gh args: %s\n' "$*" >&2
exit 1
`)

	markerPath := filepath.Join(dir, "markers", "reruns.json")
	cmd := exec.Command(civmctlBin,
		"runner", "watchdog",
		"--execute",
		"--repos=auto",
		"--rerun-network-failures",
		"--max-run-age=6h",
		"--restart-delay=0s",
		"--marker-path="+markerPath,
		"--hooks-dir="+filepath.Join(dir, "hooks"),
		"--runner-glob="+filepath.Join(dir, "actions-runner-*"),
		"--civmctl-path="+civmctlBin,
		"--json",
	)
	cmd.Env = append(os.Environ(),
		"HOME="+dir,
		"PATH="+binDir+":/usr/bin:/bin",
		"CIVM_FAKE_LOG="+logPath,
		"CIVM_FAKE_RUNNER_DIR="+runnerDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runner watchdog fake failed: %v\n%s", err, out)
	}
	var report struct {
		Metrics struct {
			RunsConsidered  int `json:"runs_considered"`
			RerunsTriggered int `json:"reruns_triggered"`
			RerunsSkipped   int `json:"reruns_skipped"`
		} `json:"metrics"`
		Events []integrationEvent `json:"events"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !hasIntegrationEvent(report.Events, "runner-restarted") || !hasIntegrationEvent(report.Events, "rerun-triggered") {
		t.Fatalf("events = %+v, want runner-restarted and rerun-triggered\n%s", report.Events, out)
	}
	if report.Metrics.RunsConsidered != 1 || report.Metrics.RerunsTriggered != 1 || report.Metrics.RerunsSkipped != 0 {
		t.Fatalf("metrics = %+v, want considered=1 triggered=1 skipped=0", report.Metrics)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logData)
	if !strings.Contains(log, "sudo systemctl restart actions.runner.acme-civm.civm-self.service") {
		t.Fatalf("missing restart command in log:\n%s", log)
	}
	if !strings.Contains(log, "gh run rerun 99 --repo acme/civm --failed") {
		t.Fatalf("missing rerun command in log:\n%s", log)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(marker), `"99/abc123"`) {
		t.Fatalf("marker missing run/head:\n%s", marker)
	}
}

func TestIntegration_RunnerWatchdogNetworkDownDoesNotMutate(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil { //nolint:gosec // G301: temp PATH dir needs executable bit.
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "commands.log")
	writeFakeCommand(t, binDir, "git", `#!/usr/bin/env bash
printf 'git %s\n' "$*" >> "$CIVM_FAKE_LOG"
exit 42
`)
	for _, name := range []string{"gh", "systemctl", "sudo", "ps"} {
		writeFakeCommand(t, binDir, name, `#!/usr/bin/env bash
printf '`+name+` %s\n' "$*" >> "$CIVM_FAKE_LOG"
exit 0
`)
	}

	cmd := exec.Command(civmctlBin,
		"runner", "watchdog",
		"--execute",
		"--repos=auto",
		"--rerun-network-failures",
		"--marker-path="+filepath.Join(dir, "reruns.json"),
		"--hooks-dir="+filepath.Join(dir, "hooks"),
		"--runner-glob="+filepath.Join(dir, "actions-runner-*"),
		"--json",
	)
	cmd.Env = append(os.Environ(),
		"HOME="+dir,
		"PATH="+binDir+":/usr/bin:/bin",
		"CIVM_FAKE_LOG="+logPath,
	)
	out, err := cmd.CombinedOutput()
	if got := integrationExitCode(err); got != 1 {
		t.Fatalf("exit = %d, want 1 err=%v\n%s", got, err, out)
	}
	var report struct {
		Events []integrationEvent `json:"events"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !hasIntegrationEvent(report.Events, "network-down") {
		t.Fatalf("events = %+v, want network-down", report.Events)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logData)
	for _, forbidden := range []string{"sudo ", "gh ", "systemctl ", "ps "} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("network-down mutated or probed after failure (%q) log:\n%s", forbidden, log)
		}
	}
}

func writeFakeCommand(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0755); err != nil { //nolint:gosec // G306: fake command must be executable.
		t.Fatal(err)
	}
}

func integrationExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func hasIntegrationEvent(events []integrationEvent, want string) bool {
	for _, event := range events {
		if event.Event == want {
			return true
		}
	}
	return false
}
