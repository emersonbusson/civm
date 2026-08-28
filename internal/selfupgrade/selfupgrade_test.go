package selfupgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_DryRunDoesNotBuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "src")
	if err := os.MkdirAll(source, 0755); err != nil { //nolint:gosec // G301: tempdir test
		t.Fatal(err)
	}
	called := 0
	opts := Options{
		SourceDir: source,
		Target:    filepath.Join(dir, "civmctl"),
		Execute:   false,
		BuildFn: func(context.Context, string, string) error {
			called++
			return nil
		},
	}
	res := Run(context.Background(), opts)
	if called != 0 {
		t.Errorf("dry-run called BuildFn %d times", called)
	}
	if res.Executed || res.Swapped {
		t.Errorf("dry-run reports execution: %+v", res)
	}
	if res.Error != "" {
		t.Errorf("unexpected error: %s", res.Error)
	}
}

func TestRun_DryRunReportsMissingSourceDir(t *testing.T) {
	t.Parallel()
	opts := Options{
		SourceDir: "/non/existent/source",
		Target:    "/tmp/civmctl",
		Execute:   false,
	}
	res := Run(context.Background(), opts)
	if res.Error == "" {
		t.Error("expected error for missing source_dir")
	}
}

func TestRun_ExecuteBuildsVerifiesSwaps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "src")
	if err := os.MkdirAll(source, 0755); err != nil { //nolint:gosec // G301: tempdir test
		t.Fatal(err)
	}
	target := filepath.Join(dir, "civmctl")
	// Pre-populate target so OldSize is captured.
	if err := os.WriteFile(target, []byte("old-binary"), 0755); err != nil { //nolint:gosec // G302/G306: test binary
		t.Fatal(err)
	}
	verifyCalled := false
	opts := Options{
		SourceDir: source,
		Target:    target,
		Execute:   true,
		BuildFn: func(_ context.Context, sourceDir, output string) error {
			if sourceDir != source {
				return fmt.Errorf("unexpected source: %s", sourceDir)
			}
			return os.WriteFile(output, []byte("new-binary-content"), 0755) //nolint:gosec // G302/G306: test binary
		},
		VerifyFn: func(path string) error { verifyCalled = true; _ = path; return nil },
	}
	res := Run(context.Background(), opts)
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !res.Verified || !res.Swapped {
		t.Errorf("expected verified+swapped, got %+v", res)
	}
	if !verifyCalled {
		t.Error("VerifyFn was not called")
	}
	if res.OldSize != int64(len("old-binary")) {
		t.Errorf("OldSize = %d, want %d", res.OldSize, len("old-binary"))
	}
	if res.NewSize != int64(len("new-binary-content")) {
		t.Errorf("NewSize = %d, want %d", res.NewSize, len("new-binary-content"))
	}
	// Verify the target was atomically replaced.
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-binary-content" {
		t.Errorf("target content = %q", data)
	}
	// No leftover .civmctl.new in target dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".civmctl.new") {
			t.Errorf("leftover temp build: %s", e.Name())
		}
	}
}

func TestRun_BuildFailureLeavesTargetUntouched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "src")
	_ = os.MkdirAll(source, 0755) //nolint:gosec // G104/G301: tempdir test setup
	target := filepath.Join(dir, "civmctl")
	originalContent := []byte("OLD-original")
	_ = os.WriteFile(target, originalContent, 0755) //nolint:gosec // G104/G302/G306: test binary
	opts := Options{
		SourceDir: source,
		Target:    target,
		Execute:   true,
		BuildFn:   func(context.Context, string, string) error { return errors.New("compile failed") },
	}
	res := Run(context.Background(), opts)
	if res.Error == "" {
		t.Error("expected build error")
	}
	if res.Swapped {
		t.Error("should not have swapped on build failure")
	}
	// Target must still be the original.
	data, _ := os.ReadFile(target)
	if string(data) != string(originalContent) {
		t.Errorf("target was modified: %q", data)
	}
}

func TestRun_VerifyFailureCleansTempAndAborts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "src")
	_ = os.MkdirAll(source, 0755) //nolint:gosec // G104/G301: tempdir test setup
	target := filepath.Join(dir, "civmctl")
	originalContent := []byte("OLD")
	_ = os.WriteFile(target, originalContent, 0755) //nolint:gosec // G104/G302/G306: test binary
	opts := Options{
		SourceDir: source,
		Target:    target,
		Execute:   true,
		BuildFn: func(_ context.Context, _, output string) error {
			return os.WriteFile(output, []byte("garbage"), 0755) //nolint:gosec // G302/G306: test binary
		},
		VerifyFn: func(string) error { return errors.New("not an ELF binary") },
	}
	res := Run(context.Background(), opts)
	if res.Error == "" {
		t.Error("expected verify error")
	}
	if res.Swapped || res.Verified {
		t.Errorf("must not swap on verify failure: %+v", res)
	}
	// Original target intact.
	data, _ := os.ReadFile(target)
	if string(data) != string(originalContent) {
		t.Errorf("target was modified: %q", data)
	}
	// Temp file cleaned.
	if _, err := os.Stat(res.BuiltAt); err == nil {
		t.Errorf("temp build leaked: %s", res.BuiltAt)
	}
}

func TestDefaultVerify(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// A binary whose version-pins prints output and exits 0 verifies.
	good := filepath.Join(dir, "good")
	writeStubBinary(t, good, "echo civmctl version pins\nexit 0")
	if err := defaultVerify(good); err != nil {
		t.Fatalf("good binary should verify: %v", err)
	}

	// Pair the positive with the failures the gate must catch: a non-zero exit
	// (a build that compiles but breaks in command dispatch) is rejected...
	bad := filepath.Join(dir, "bad")
	writeStubBinary(t, bad, "echo boom 1>&2\nexit 3")
	if err := defaultVerify(bad); err == nil {
		t.Fatal("non-zero exit should fail verify")
	}

	// ...and so is empty output, which --help-style smoke would have missed.
	empty := filepath.Join(dir, "empty")
	writeStubBinary(t, empty, "exit 0")
	if err := defaultVerify(empty); err == nil {
		t.Fatal("empty output should fail verify")
	}
}

func writeStubBinary(t *testing.T, path, body string) {
	t.Helper()
	script := "#!/usr/bin/env bash\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test stub must be executable
		t.Fatalf("write stub binary: %v", err)
	}
}

func TestRun_RenameFailureCleansTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "src")
	_ = os.MkdirAll(source, 0755) //nolint:gosec // G104/G301: tempdir test setup
	target := filepath.Join(dir, "civmctl")
	opts := Options{
		SourceDir: source,
		Target:    target,
		Execute:   true,
		BuildFn: func(_ context.Context, _, output string) error {
			return os.WriteFile(output, []byte("ok"), 0755) //nolint:gosec // G302/G306: test binary
		},
		VerifyFn: func(string) error { return nil },
		RenameFn: func(string, string) error { return errors.New("EXDEV") },
	}
	res := Run(context.Background(), opts)
	if res.Error == "" {
		t.Error("expected rename error")
	}
	if _, err := os.Stat(res.BuiltAt); err == nil {
		t.Errorf("temp leaked on rename failure: %s", res.BuiltAt)
	}
}

func TestRender_JSONAndText(t *testing.T) {
	t.Parallel()
	res := Result{
		Executed: true, SourceDir: "/opt/civm", Target: "/usr/local/bin/civmctl",
		BuiltAt: "/tmp/.civmctl.new", Verified: true, Swapped: true,
		OldSize: 1000, NewSize: 1200,
	}
	var buf strings.Builder
	if err := RenderJSON(&buf, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"swapped": true`) {
		t.Errorf("JSON missing swapped: %s", buf.String())
	}
	buf.Reset()
	RenderText(&buf, res)
	for _, want := range []string{"EXECUTE", "/opt/civm", "/usr/local/bin/civmctl", "1200"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("text missing %q: %s", want, buf.String())
		}
	}
}

func TestRender_TextDryRunAndError(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	RenderText(&buf, Result{Target: "/usr/local/bin/civmctl", Error: "boom"})
	out := buf.String()
	if !strings.Contains(out, "DRY-RUN") || !strings.Contains(out, "boom") {
		t.Errorf("dry-run/error render wrong: %s", out)
	}
}
