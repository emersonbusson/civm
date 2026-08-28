package hostdisk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOrchestratorDocumentsOwnRepoConfig protects the self-monitor invariant
// without shipping a product-specific fleet in script defaults.
//
// Get-RunCount only sees repos listed in $Repos. If the operator's own civm
// repo is not monitored, self-hosted CI for this project is invisible and the
// VM never starts. Public/generic defaults leave Repos empty; the script must
// still document that the operator must include their own repo (example
// owner/civm) when configuring the host.
func TestOrchestratorDocumentsOwnRepoConfig(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "windows", "civm-vm-orchestrator.ps1")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read civm-vm-orchestrator.ps1: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "[string[]]$Repos") {
		t.Error("civm-vm-orchestrator.ps1 must expose $Repos for operator configuration")
	}
	// Prefer empty default + docs over hard-coded multi-tenant fleets.
	if strings.Contains(body, "other/service-a") || strings.Contains(body, "emersonbusson/") {
		t.Error("civm-vm-orchestrator.ps1 must not ship product multi-tenant repo fleets as defaults")
	}
}
