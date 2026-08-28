package diskaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectReportsSafeDiskOwners(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSizedFile(t, filepath.Join(home, ".cache", "go-build", "obj"), 12)
	writeSizedFile(t, filepath.Join(home, "go", "pkg", "mod", "dep"), 8)
	writeSizedFile(t, filepath.Join(home, "codespace", "civm", "repo"), 20)
	writeSizedFile(t, filepath.Join(home, "actions-runner-a", "_work", "repo", "file"), 5)
	writeSizedFile(t, filepath.Join(home, "actions-runner-a", "_work", "_tool", "go"), 30)
	writeSizedFile(t, filepath.Join(home, "actions-runner-a", "_work", "_actions", "checkout"), 10)

	opts := DefaultOptions()
	opts.HomeDir = home
	opts.RunnerWorkGlob = filepath.Join(home, "actions-runner-*", "_work")
	opts.IncludeSystem = false
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("76MB (12%)\n"), nil
	}

	report := Collect(context.Background(), opts)
	for _, want := range []string{"runner_work", "runner_tool_cache", "runner_action_cache", "home_cache", "go_pkg", "codespace", "docker_reclaimable"} {
		if !hasEntry(report, want) {
			t.Fatalf("missing %s in %+v", want, report.Entries)
		}
	}
	if report.Entries[0].Name != "docker_reclaimable" {
		t.Fatalf("largest entry = %+v, want docker first", report.Entries[0])
	}
}

func TestCollectLimitAndJSON(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSizedFile(t, filepath.Join(home, ".cache", "x"), 1)
	writeSizedFile(t, filepath.Join(home, "codespace", "x"), 2)

	opts := DefaultOptions()
	opts.HomeDir = home
	opts.RunnerWorkGlob = filepath.Join(home, "missing-*", "_work")
	opts.IncludeSystem = false
	opts.IncludeDocker = false
	opts.Limit = 1
	report := Collect(context.Background(), opts)
	if len(report.Entries) != 1 {
		t.Fatalf("len(entries)=%d, want 1", len(report.Entries))
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	var parsed Report
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(parsed.Entries) != 1 {
		t.Fatalf("parsed entries=%d, want 1", len(parsed.Entries))
	}
}

func TestCollectUsesDUWhenAvailable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSizedFile(t, filepath.Join(home, "codespace", "x"), 2)

	opts := DefaultOptions()
	opts.HomeDir = home
	opts.RunnerWorkGlob = filepath.Join(home, "missing-*", "_work")
	opts.IncludeSystem = false
	opts.IncludeDocker = false
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "du" && len(args) == 2 && args[1] == filepath.Join(home, "codespace") {
			return []byte("123\t" + args[1] + "\n"), nil
		}
		return nil, os.ErrNotExist
	}

	report := Collect(context.Background(), opts)
	for _, entry := range report.Entries {
		if entry.Name == "codespace" && entry.Bytes != 123 {
			t.Fatalf("codespace bytes=%d, want du size 123", entry.Bytes)
		}
	}
}

func TestCollectFallsBackToWalkWhenDUFails(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSizedFile(t, filepath.Join(home, "codespace", "x"), 2)

	opts := DefaultOptions()
	opts.HomeDir = home
	opts.RunnerWorkGlob = filepath.Join(home, "missing-*", "_work")
	opts.IncludeSystem = false
	opts.IncludeDocker = false
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.ErrNotExist
	}

	report := Collect(context.Background(), opts)
	for _, entry := range report.Entries {
		if entry.Name == "codespace" && entry.Bytes != 2 {
			t.Fatalf("codespace bytes=%d, want fallback walk size 2", entry.Bytes)
		}
	}
}

func TestCollectDoesNotWalkAfterContextDeadline(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSizedFile(t, filepath.Join(home, "codespace", "x"), 2)

	ctx, cancel := context.WithCancel(context.Background())
	opts := DefaultOptions()
	opts.HomeDir = home
	opts.RunnerWorkGlob = filepath.Join(home, "missing-*", "_work")
	opts.IncludeSystem = false
	opts.IncludeDocker = false
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		cancel()
		return nil, context.Canceled
	}
	opts.WalkFn = func(string, fs.WalkDirFunc) error {
		t.Fatal("WalkFn should not run after context cancellation")
		return nil
	}

	report := Collect(ctx, opts)
	for _, entry := range report.Entries {
		if entry.Name == "codespace" && entry.Status != "partial" {
			t.Fatalf("codespace status=%s, want partial", entry.Status)
		}
	}
}

func TestCollectReportsMissingFileAndStatErrors(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	codespace := filepath.Join(home, "codespace")
	writeSizedFile(t, codespace, 11)
	statErr := errors.New("permission denied")

	opts := DefaultOptions()
	opts.HomeDir = home
	opts.RunnerWorkGlob = filepath.Join(home, "missing-*", "_work")
	opts.IncludeSystem = false
	opts.IncludeDocker = false
	opts.StatFn = func(path string) (fs.FileInfo, error) {
		if path == filepath.Join(home, ".cache") {
			return nil, statErr
		}
		return os.Stat(path)
	}

	report := Collect(context.Background(), opts)

	got := entriesByName(report)
	if got["codespace"].Bytes != 11 || got["codespace"].Status != "ok" {
		t.Fatalf("codespace entry=%+v, want file size with ok status", got["codespace"])
	}
	if got["go_pkg"].Status != "missing" {
		t.Fatalf("go_pkg status=%q, want missing", got["go_pkg"].Status)
	}
	if got["home_cache"].Status != "error" || !strings.Contains(got["home_cache"].Error, statErr.Error()) {
		t.Fatalf("home_cache entry=%+v, want stat error", got["home_cache"])
	}
}

func TestCollectReportsWalkPartialErrors(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSizedFile(t, filepath.Join(home, "codespace", "x"), 2)
	walkErr := errors.New("walk stopped")

	opts := DefaultOptions()
	opts.HomeDir = home
	opts.RunnerWorkGlob = filepath.Join(home, "missing-*", "_work")
	opts.IncludeSystem = false
	opts.IncludeDocker = false
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.ErrNotExist
	}
	opts.WalkFn = func(root string, fn fs.WalkDirFunc) error {
		if root == filepath.Join(home, "codespace") {
			return walkErr
		}
		return filepath.WalkDir(root, fn)
	}

	report := Collect(context.Background(), opts)

	entry := entriesByName(report)["codespace"]
	if entry.Status != "partial" || !strings.Contains(entry.Error, walkErr.Error()) {
		t.Fatalf("codespace entry=%+v, want partial walk error", entry)
	}
}

func TestApplyDefaultsForZeroOptions(t *testing.T) {
	t.Parallel()
	opts := Options{Limit: -1}
	applyDefaults(&opts)

	if opts.HomeDir == "" {
		t.Fatal("HomeDir should be defaulted")
	}
	if opts.RunnerWorkGlob == "" {
		t.Fatal("RunnerWorkGlob should be defaulted")
	}
	if opts.Limit != 20 {
		t.Fatalf("Limit=%d, want 20", opts.Limit)
	}
	if opts.GlobFn == nil || opts.WalkFn == nil || opts.StatFn == nil || opts.RunFn == nil {
		t.Fatalf("default funcs not installed: %+v", opts)
	}
}

func TestRenderTextIncludesEntries(t *testing.T) {
	t.Parallel()
	report := Report{Entries: []Entry{{Name: "codespace", Kind: "workspace_clones", Path: "/home/x/codespace", Bytes: 1024, Status: "partial", Error: "read denied while walking"}}}
	report.TotalBytes = 1024
	var buf bytes.Buffer
	RenderText(&buf, report)
	out := buf.String()
	for _, want := range []string{"NAME", "codespace", "1.0 kB", "partial", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text missing %q:\n%s", want, out)
		}
	}
}

func TestDockerEntryReportsCommandError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("docker unavailable")
	opts := DefaultOptions()
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, wantErr
	}

	entry := dockerEntry(context.Background(), opts)

	if entry.Status != "error" || !strings.Contains(entry.Error, wantErr.Error()) {
		t.Fatalf("entry=%+v, want docker error", entry)
	}
}

func TestParseHumanBytes(t *testing.T) {
	t.Parallel()
	cases := map[string]int64{
		"1GB":   1 << 30,
		"512MB": 512 << 20,
		"128kB": 128 << 10,
		"42B":   42,
		"n/a":   0,
		"76MB":  76 << 20,
		"1.5GB": int64(1.5 * float64(1<<30)),
		"10 KB": 10 << 10,
	}
	for in, want := range cases {
		if got := parseHumanBytes(in); got != want {
			t.Fatalf("parseHumanBytes(%q)=%d, want %d", in, got, want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{
		42:      "42 B",
		1 << 10: "1.0 kB",
		2 << 20: "2.0 MB",
		3 << 30: "3.0 GB",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Fatalf("FormatBytes(%d)=%q, want %q", in, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate short=%q, want unchanged", got)
	}
	if got := truncate("abcdefghijk", 5); got != "abcd\u2026" {
		t.Fatalf("truncate long=%q, want truncated", got)
	}
}

func hasEntry(report Report, name string) bool {
	for _, entry := range report.Entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

func entriesByName(report Report) map[string]Entry {
	out := make(map[string]Entry, len(report.Entries))
	for _, entry := range report.Entries {
		out[entry.Name] = entry
	}
	return out
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0600); err != nil {
		t.Fatal(err)
	}
}
