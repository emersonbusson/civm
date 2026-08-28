package hostdisk

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func executablePowerShell(body string) string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func TestGenerationBoundaryNeverForcesActiveWork(t *testing.T) {
	body := executablePowerShell(readWindowsScript(t, "civm-vm-orchestrator.ps1"))
	if regexp.MustCompile(`(?m)\bStop-VM\b[^\r\n]*\s-Force\b`).MatchString(body) {
		t.Error("generation boundary must not force Stop-VM in executable PowerShell")
	}
	for _, forbidden := range []string{
		"push_wave_force_compact",
		"skip_clean",
		"Resolve-PushWaveCompact",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("generation boundary must not contain %q in executable PowerShell", forbidden)
		}
	}
	for _, required := range []string{
		"civmctl idle-check",
		"sudo -n /usr/local/bin/civm-generation-boundary --check",
		"sudo -n /usr/local/bin/civm-generation-boundary prepare",
		"sudo -n /usr/local/bin/civm-generation-boundary resume",
		"Wait-GuestOff",
		"flock -n",
		"--kill-after=5s",
		"Invoke-PrepareGeneration",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("generation boundary must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"sudo -n civmctl maintenance",
		"sudo -n shutdown",
		"sudo docker",
		"sudo fstrim",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("generation boundary must not grant or use broad privileged command %q", forbidden)
		}
	}
	if !strings.Contains(body, "civm-generation-boundary warn-clean") {
		t.Error("online warn cleanup must also use the fixed generation-boundary wrapper")
	}
	if !strings.Contains(body, "set -euo pipefail\nset -a") {
		t.Error("guest reaper fallback must preserve reap-runs failures through its pipeline")
	}
}

func TestGenerationBoundaryUsesStrictEightyGiBFloor(t *testing.T) {
	orchestrator := executablePowerShell(readWindowsScript(t, "civm-vm-orchestrator.ps1"))
	if !strings.Contains(orchestrator, "$AdmitFloorGB = 80") {
		t.Error("orchestrator must configure AdmitFloorGB = 80")
	}
	decision := executablePowerShell(readWindowsScript(t, "civm-orchestrator-decision.ps1"))
	if !strings.Contains(decision, "[int]$AdmitFloorGB = 80") {
		t.Error("decision module must default AdmitFloorGB to 80")
	}
	for _, forbidden := range []string{"AdmitReclaimAttempts", "panic_compact"} {
		if strings.Contains(decision, forbidden) {
			t.Errorf("decision module must not retain capacity bypass %q", forbidden)
		}
	}
}

func TestGenerationBoundaryWrapperUsesFixedPrivilegedExecutables(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "bin", "civm-generation-boundary")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(data)
	for _, required := range []string{
		"#!/bin/bash",
		"readonly CIVMCTL=/usr/local/bin/civmctl",
		"readonly DOCKER=/usr/bin/docker",
		"readonly JOURNALCTL=/usr/bin/journalctl",
		"readonly FSTRIM=/usr/sbin/fstrim",
		"readonly SYSTEMCTL=/usr/bin/systemctl",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("generation wrapper must contain fixed executable %q", required)
		}
	}
	for _, forbidden := range []string{"eval ", `"$@"`, "/bin/sh -c"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("generation wrapper must not contain caller-controlled execution %q", forbidden)
		}
	}
}

func TestGenerationAdmissionCannotBypassQueue(t *testing.T) {
	body := executablePowerShell(readWindowsScript(t, "civm-vm-orchestrator.ps1"))
	mainAt := strings.LastIndex(body, "try {\n    $state = Get-State")
	if mainAt < 0 {
		t.Fatal("orchestrator main decision entrypoint is missing")
	}
	main := body[mainAt:]
	for _, forbidden := range []string{"Start-VM", "Publish-CurrentContext"} {
		if strings.Contains(main, forbidden) {
			t.Errorf("generic main flow must not %s; generation queue owns admission", forbidden)
		}
	}
	queueAt := strings.Index(body, "function Invoke-PrGenerationQueue")
	if queueAt < 0 || !strings.Contains(body[queueAt:mainAt], "Invoke-PrepareGeneration") || !strings.Contains(body[queueAt:mainAt], "Publish-CurrentContext") {
		t.Error("generation queue must prepare and publish the exact context")
	}
	if !strings.Contains(body, "generation_context_legacy_ignored") {
		t.Error("legacy pr-only context must fail closed instead of being treated as a completed generation")
	}
}
