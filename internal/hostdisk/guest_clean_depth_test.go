package hostdisk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerationBoundaryCleanIsDeep freezes the root-side clean-state contract.
// It removes every regenerable runner/cache/Docker artifact after a strict drain,
// while retaining only the VM image and runner configuration needed to resume.
func TestGenerationBoundaryCleanIsDeep(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "bin", "civm-generation-boundary")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read civm-vm-orchestrator.ps1: %v", err)
	}
	src := string(data)
	mustContain := map[string]string{
		"_diag":                       "remove runner diagnostic logs",
		"_work":                       "remove all runner work state",
		"-mindepth 1 -maxdepth 1":     "remove complete work/cache children",
		"clean_home_caches":           "remove package-manager/compiler caches",
		`"$JOURNALCTL" --vacuum`:      "vacuum the journal",
		`"$DOCKER" system prune -af`:  "remove unused images, containers and volumes",
		`"$DOCKER" builder prune -af`: "prune the builder cache aggressively",
		`"$DOCKER" ps -aq`:            "block admission if a container remains",
		`"$FSTRIM" -av`:               "mark released blocks for compaction",
	}
	for needle, why := range mustContain {
		if !strings.Contains(src, needle) {
			t.Errorf("generation-boundary wrapper must %s (missing %q)", why, needle)
		}
	}
	for _, forbidden := range []string{"_tool and _actions stay", "-mindepth 3 -maxdepth 3"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("generation-boundary wrapper must not preserve stale work state %q", forbidden)
		}
	}
}
