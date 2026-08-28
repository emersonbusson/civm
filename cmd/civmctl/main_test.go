package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/runner"
	"github.com/emersonbusson/civm/internal/specs"
)

func TestRenderSpecJSONValid(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if code := renderSpecJSON(specs.Ubuntu2404(), &buf); code != 0 {
		t.Fatalf("renderSpecJSON code = %d", code)
	}
	var parsed struct {
		OSDistro string `json:"os_distro"`
		Tools    []struct {
			Name      string `json:"name"`
			Preferred string `json:"preferred"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json invalido: %v", err)
	}
	if parsed.OSDistro != "Ubuntu" || len(parsed.Tools) == 0 {
		t.Fatalf("json inesperado: distro=%q tools=%d", parsed.OSDistro, len(parsed.Tools))
	}
}

func TestRunVersionPinsBadFlag(t *testing.T) {
	t.Parallel()
	if code := runVersionPins([]string{"--bad"}); code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestPrintHelpUsesExplicitHeavyDockerAdmission(t *testing.T) {
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = oldStdout
	}()

	printHelp()
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read help: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}

	help := string(output)
	if !strings.Contains(help, "civmctl admit --weight heavy --exclusive docker --wait-minutes 30 --exec -- make up-local") {
		t.Fatalf("help must document explicit Docker-heavy admission, got:\n%s", help)
	}
	if strings.Contains(help, "civmctl admit --weight auto --exclusive docker") {
		t.Fatalf("help must not present auto weight as the Docker-heavy contract, got:\n%s", help)
	}
}

func TestRunBillingRejectsBadRepoBeforeGh(t *testing.T) {
	t.Parallel()
	if code := runBilling([]string{"--repo=bad/repo;rm"}); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestBootstrapEverythingHelpers(t *testing.T) {
	t.Parallel()
	if got := execFlag(true); got != "--execute" {
		t.Fatalf("execFlag(true) = %q", got)
	}
	if got := execFlag(false); got != "" {
		t.Fatalf("execFlag(false) = %q", got)
	}
	if got := boolStr(true); got != "true" {
		t.Fatalf("boolStr(true) = %q", got)
	}
	if got := joinArgs([]string{"cp", "a", "b"}); got != "cp a b" {
		t.Fatalf("joinArgs = %q", got)
	}
	steps := buildBootstrapEverythingSteps("/opt/civm/deploy/systemd", true, true, true, true, false)
	if len(steps) == 0 || !bytes.Contains([]byte(steps[0].WouldDo), []byte("/usr/local/bin/civmctl")) {
		t.Fatalf("bootstrap-everything deve validar /usr/local/bin/civmctl, step=%+v", steps)
	}
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.Name)
	}
	if !slices.Contains(names, "cp_civmctl-run-reaper_service") ||
		!slices.Contains(names, "cp_civmctl-run-reaper_timer") {
		t.Fatalf("bootstrap-everything omitiu units do reaper: %v", names)
	}
}

func TestSplitCSV(t *testing.T) {
	t.Parallel()
	got := splitCSV("acme/civm, other/peer,,")
	if len(got) != 2 || got[0] != "acme/civm" || got[1] != "other/peer" {
		t.Fatalf("splitCSV = %#v", got)
	}
}

func TestConfigureDoctorReposModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw       string
		wantInfer bool
		wantRepos []string
	}{
		{raw: "auto", wantInfer: true},
		{raw: "none", wantInfer: false, wantRepos: nil},
		{raw: "", wantInfer: false, wantRepos: nil},
		{raw: "default", wantInfer: false, wantRepos: nil},
		{raw: "acme/api, acme/web", wantInfer: false, wantRepos: []string{"acme/api", "acme/web"}},
	}
	for _, c := range cases {
		opts := defaultDoctorCLIOptions()
		if err := configureDoctorRepos(c.raw, &opts); err != nil {
			t.Fatalf("configureDoctorRepos(%q) err = %v", c.raw, err)
		}
		if opts.InferRepos != c.wantInfer || strings.Join(opts.Repos, ",") != strings.Join(c.wantRepos, ",") {
			t.Fatalf("configureDoctorRepos(%q) = infer=%v repos=%v, want infer=%v repos=%v",
				c.raw, opts.InferRepos, opts.Repos, c.wantInfer, c.wantRepos)
		}
	}
}

func TestConfigureRunnerWatchdogReposModes(t *testing.T) {
	t.Parallel()
	opts := runner.DefaultWatchdogOptions()
	if err := configureRunnerWatchdogRepos("auto", &opts); err != nil {
		t.Fatalf("auto err = %v", err)
	}
	if !opts.InferRepos || len(opts.Repos) != 0 {
		t.Fatalf("auto = infer=%v repos=%v, want infer true nil", opts.InferRepos, opts.Repos)
	}

	opts = runner.DefaultWatchdogOptions()
	if err := configureRunnerWatchdogRepos("acme/civm, other/peer", &opts); err != nil {
		t.Fatalf("list err = %v", err)
	}
	if opts.InferRepos || strings.Join(opts.Repos, ",") != "acme/civm,other/peer" {
		t.Fatalf("list = infer=%v repos=%v", opts.InferRepos, opts.Repos)
	}

	opts = runner.DefaultWatchdogOptions()
	if err := configureRunnerWatchdogRepos("", &opts); err == nil {
		t.Fatalf("empty --repos should error")
	}
}

func TestRunRunnerWatchdogRejectsNonPositiveMaxRunAge(t *testing.T) {
	t.Parallel()
	if code := runRunnerWatchdog([]string{"--max-run-age=0s"}); code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestRunRunnerWatchdogRejectsNonPositiveQueueStallDwell(t *testing.T) {
	t.Parallel()
	if code := runRunnerWatchdog([]string{"--queue-stall-dwell=0s"}); code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestRunCleanupRejectsManagedVolumesWithoutDocker(t *testing.T) {
	t.Parallel()
	if code := runCleanup([]string{"--managed-volumes", "--no-docker"}); code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestRunDiskAuditRejectsInvalidBounds(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--limit=0"},
		{"--timeout=0"},
	} {
		if code := runDiskAudit(args); code != exitUsage {
			t.Fatalf("runDiskAudit(%v) code = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestHookEventFromArgv0(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arg0  string
		event string
		ok    bool
	}{
		{"/opt/civm/hooks/job-started", "job-started", true},
		{"/opt/civm/hooks/job-completed", "job-completed", true},
		{"job-started", "job-started", true},
		{"./job-completed", "job-completed", true},
		// Legacy .sh suffix tolerated during transitional installs.
		{"/opt/civm/hooks/job-started.sh", "job-started", true},
		{"/usr/local/bin/civmctl", "", false},
		{"civmctl", "", false},
		{"", "", false},
		{"job-other", "", false},
	}
	for _, c := range cases {
		event, ok := hookEventFromArgv0(c.arg0)
		if ok != c.ok || event != c.event {
			t.Errorf("hookEventFromArgv0(%q) = (%q,%v); want (%q,%v)",
				c.arg0, event, ok, c.event, c.ok)
		}
	}
}

// FuzzSplitCSV enforces the post-condition that splitCSV's output never
// contains empty entries, never contains leading/trailing whitespace, and
// never contains a comma. Downstream callers (runner add/remove, ci local-report)
// pass each entry to civm.ValidateRepo, which is regex-strict; we only test
// the trim+filter contract here.
func FuzzSplitCSV(f *testing.F) {
	seeds := []string{
		"",
		"acme/civm",
		"acme/civm,other/x",
		",,",
		" acme/civm , other/x ",
		"a,,b,,,c",
		"\tacme/civm\n",
		"single-no-slash",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		out := splitCSV(raw)
		for i, part := range out {
			if part == "" {
				t.Fatalf("splitCSV(%q)[%d] is empty", raw, i)
			}
			if strings.ContainsRune(part, ',') {
				t.Fatalf("splitCSV(%q)[%d] contains comma: %q", raw, i, part)
			}
			if part != strings.TrimSpace(part) {
				t.Fatalf("splitCSV(%q)[%d] is not trimmed: %q", raw, i, part)
			}
		}
	})
}
