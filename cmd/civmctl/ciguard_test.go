package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCIGuardCompose(t *testing.T, root, content string) {
	t.Helper()
	path := filepath.Join(root, "infra", "docker-compose.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestRunCIGuardBadFlag(t *testing.T) {
	t.Parallel()
	if code := runCIGuard([]string{"--bad"}); code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestRunCIGuardInvalidMode(t *testing.T) {
	t.Parallel()
	if code := runCIGuard([]string{"--mode=block"}); code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
}

func TestRunCIGuardReportNeverFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCIGuardCompose(t, root, "services:\n  db:\n    container_name: x\n")
	if code := runCIGuard([]string{"--repo-root=" + root, "--mode=report"}); code != 0 {
		t.Fatalf("report code = %d, want 0", code)
	}
}

func TestRunCIGuardEnforceFailsOnViolation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCIGuardCompose(t, root, "services:\n  db:\n    container_name: x\n")
	if code := runCIGuard([]string{"--repo-root=" + root, "--mode=enforce"}); code != 1 {
		t.Fatalf("enforce code = %d, want 1", code)
	}
}

func TestRunCIGuardEnforcePassesWhenClean(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCIGuardCompose(t, root, "services:\n  web:\n    ports:\n      - \"${CIVM_PORT_BASE}:80\"\n")
	if code := runCIGuard([]string{"--repo-root=" + root, "--mode=enforce", "--json"}); code != 0 {
		t.Fatalf("clean enforce code = %d, want 0", code)
	}
}
