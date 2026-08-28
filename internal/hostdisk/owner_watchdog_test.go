package hostdisk

import (
	"strings"
	"testing"
)

// The live watchdog kept checking the disabled PowerShell orchestrator after
// the C# owner cutover. That made every healthy tick report DRIFT and left the
// actual owner heartbeat unobserved during the peer#1609 queue stall.
func TestOwnerWatchdogTracksTheSingleActiveOwner(t *testing.T) {
	body := readWindowsScript(t, "civm-watchdog.ps1")

	for _, token := range []string{
		"civm-host-orchestrator",
		"civm-vm-orchestrator",
		"$activeOwnerCount -ne 1",
		"V:\\civm-host-shadow.jsonl",
		"HeartbeatMaxAgeMinutes = 45",
		"Get-ScheduledTaskInfo",
		"processBlockedReason",
	} {
		if !strings.Contains(body, token) {
			t.Errorf("civm-watchdog.ps1 must contain %q", token)
		}
	}

	for _, forbidden := range []string{
		"Enable-ScheduledTask",
		"Start-ScheduledTask",
		"Disable-ScheduledTask",
		"if ($orchState -ne 'Ready')",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("civm-watchdog.ps1 must detect owner drift without %q", forbidden)
		}
	}
}

func TestOwnerWatchdogRegistrationIsBoundedAndVersioned(t *testing.T) {
	body := readWindowsScript(t, "register-civm-watchdog.ps1")

	for _, token := range []string{
		"civm-watchdog.ps1",
		"C:\\civm-deploy",
		"ExecutionTimeLimit",
		"MultipleInstances IgnoreNew",
		"New-ScheduledTaskTrigger -AtStartup",
		"New-TimeSpan -Minutes 20",
	} {
		if !strings.Contains(body, token) {
			t.Errorf("register-civm-watchdog.ps1 must contain %q", token)
		}
	}
}
