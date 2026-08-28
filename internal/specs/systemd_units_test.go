package specs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerWatchdogServiceLoadsCredentialEnvironmentFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "deploy", "systemd", "civmctl-runner-watchdog.service")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	unit := string(data)

	for _, want := range []string{
		"User=root",
		"EnvironmentFile=-/etc/civm/runner-watchdog.env",
		"EnvironmentFile=-/etc/civm/run-reaper.env",
		"civmctl runner watchdog --execute --repos=auto --json",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("runner-watchdog unit missing %q", want)
		}
	}
}
