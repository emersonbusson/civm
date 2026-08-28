package hostdisk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readWindowsScript(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "windows", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// SPECv3 DT-v3-2: the supervised optimize is the measurement campaign vehicle.
// It must sample LIVE V: free (Get-PSDrive, never the 10-min metrics JSON —
// red-team Finding 3) during the compaction and record scratch_high_water_gb,
// which is what calibrates the emergency admission budget. Without this number
// the emergency floor would be a guess.
func TestOptimizeScriptMeasuresScratchHighWater(t *testing.T) {
	body := readWindowsScript(t, "civm-vhdx-optimize.ps1")

	if !strings.Contains(body, "scratch_high_water_gb") {
		t.Errorf("civm-vhdx-optimize.ps1 must log scratch_high_water_gb (DT-v3-2 measurement)")
	}
	if !strings.Contains(body, "Get-PSDrive V") {
		t.Errorf("civm-vhdx-optimize.ps1 must sample LIVE V: free via Get-PSDrive during " +
			"the optimize, not the stale 10-min metrics JSON (red-team Finding 3)")
	}
}

// SPECv3 DT-v3-3 (red-team Finding 5): the two reclaim scripts use SEPARATE
// per-script locks and historically did not exclude EACH OTHER, so a supervised
// optimize and the (now pressure-cadenced) autoreclaim could Stop-VM / Optimize
// the same VHDX concurrently. Both must acquire one canonical shared lock before
// any Stop-VM, and the watchdog must consult it too. The #98 watchdog fix did
// NOT make the reclaimers mutually exclusive — this closes that gap.
func TestReclaimersShareCanonicalLock(t *testing.T) {
	const canonical = "civm-reclaim.lock"

	for _, name := range []string{"civm-vhdx-autoreclaim.ps1", "civm-vhdx-optimize.ps1"} {
		body := readWindowsScript(t, name)
		if !strings.Contains(body, canonical) {
			t.Errorf("%s must acquire the canonical %s before Stop-VM (SPECv3 DT-v3-3 mutual exclusion)",
				name, canonical)
		}
		if !strings.Contains(body, "reclaim_skip_other_active") {
			t.Errorf("%s must exit-skip (reclaim_skip_other_active) when the other reclaimer holds %s",
				name, canonical)
		}
	}

	wd := readWindowsScript(t, "register-civm-vhdx-optimize.ps1")
	if !strings.Contains(wd, canonical) {
		t.Errorf("the optimize-watchdog $MaintenanceLocks must include %s so a held canonical "+
			"lock also makes the watchdog back off (SPECv3 DT-v3-3)", canonical)
	}
}

// RF-2/DT-1 (#106) refines SPECv3 DT-v3-1: the below-headroom emergency reclaim is
// admission-gated in TWO PHASES, not floored by a pre-stop guess, and the
// Optimize-VHD it runs is UNINTERRUPTIBLE. Phase 1 only checks the budget is
// enabled; the AUTHORITATIVE gate is Phase 2 — after Stop-VM/Wait-VMState Off (when
// the ~8 GB VMRS is freed) the autoreclaim re-measures Get-PSDrive V and admits the
// Optimize only when liveFreeAfterOff − HardFloor ≥ ScratchBudget, else it skips and
// the finally re-starts the VM. It must (a) re-measure post-Off
// (autoreclaim_post_off_remeasure), (b) carry the post-Off refusal
// (autoreclaim_skip_insufficient_slack_post_off), (c) label the admitted run
// emergency_reclaim_start, and (d) NEVER contain a Stop-Job — Stop-Job does not
// abort CompactVirtualDisk, the exact unsafe mechanism the red-team killed.
func TestAutoreclaimAdmissionGate(t *testing.T) {
	body := readWindowsScript(t, "civm-vhdx-autoreclaim.ps1")

	for _, token := range []string{
		"autoreclaim_post_off_remeasure",               // RF-2/DT-1: re-measure after Off (VMRS freed)
		"autoreclaim_skip_insufficient_slack_post_off", // the post-Off gate refusal
		"vmrs_release_gb",                              // empirical VMRS-on-Off measurement
		"emergency_reclaim_start",                      // the admitted-run label
		"$ScratchBudgetGB",                             // measured budget wired in
		"$HardFloorGB",                                 // absolute hard floor wired in
		"scratch_high_water_gb",                        // DT-v3-2 measurement on the working path
		"Get-PSDrive V",                                // live V: poll, not the stale 10-min JSON
	} {
		if !strings.Contains(body, token) {
			t.Errorf("civm-vhdx-autoreclaim.ps1 must contain %q for the SPECv3 admission gate (DT-v3-1)", token)
		}
	}

	// Match a Stop-Job CALL, not a comment that explains its absence. Optimize-VHD
	// is uninterruptible (red-team Finding 2): Stop-Job does not abort the native
	// CompactVirtualDisk, so the emergency path must be admission-gated and never
	// attempt a mid-compaction abort.
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "Stop-Job") {
			t.Errorf("civm-vhdx-autoreclaim.ps1:%d must NOT call Stop-Job — Optimize-VHD is "+
				"uninterruptible (red-team Finding 2); the emergency path is admission-gated, "+
				"never aborted mid-compaction: %s", i+1, strings.TrimSpace(line))
		}
	}
}

// RF-4 / red-team BLOQUEANTE-1 (Passo 2.5): the autoreclaim's pre-Stop-VM fstrim is
// OPPORTUNISTIC discard, not a reclaim pre-requisite — Optimize-VHD -Mode Full
// compacts freed blocks offline regardless of online trim. A fail-hard `throw` on
// fstrim (EPERM, missing NOPASSWD sudoers, controller without UNMAP) would abort
// BEFORE Stop-VM and leave Phase 2 (the #106 fix) un-run — a silent spiral
// (Kahneman #13: looks deployed, does not work). It must be best-effort (WARN +
// continue), like civm-vhdx-optimize.ps1's fstrim_warn.
func TestAutoreclaimFstrimBestEffort(t *testing.T) {
	body := readWindowsScript(t, "civm-vhdx-autoreclaim.ps1")
	// Positive: the best-effort WARN path exists and the run continues to Stop-VM.
	if !strings.Contains(body, "autoreclaim_fstrim_warn") {
		t.Error("civm-vhdx-autoreclaim.ps1 must log 'autoreclaim_fstrim_warn' and continue " +
			"when fstrim fails (best-effort), not abort before Stop-VM")
	}
	// Regression guard: the old fail-hard throw must be gone, else fstrim failure
	// (e.g. missing NOPASSWD sudoers) makes the #106 fix inert and silent.
	if strings.Contains(body, `throw "fstrim failed`) {
		t.Error(`civm-vhdx-autoreclaim.ps1 must NOT fail-hard on fstrim ('throw "fstrim failed') — ` +
			"that aborts before Stop-VM and makes the #106 fix inert (red-team BLOQUEANTE-1)")
	}
}

// The supervised optimize drains the guest via `civmctl maintenance enter/exit`.
// /var/lib/civm/maintenance.lock is root-owned, and the SSH user (emdev) cannot
// open it without sudo — so the un-sudo'd call failed with "permission denied"
// BEFORE draining any runner, aborting every supervised optimize run (the reason
// the measurement-campaign vehicle never worked, found 2026-06-05). Both calls
// must use sudo -n, like the sibling `sudo fstrim`.
func TestOptimizeMaintenanceUsesSudo(t *testing.T) {
	body := readWindowsScript(t, "civm-vhdx-optimize.ps1")

	for _, cmd := range []string{
		"sudo -n civmctl maintenance enter --execute",
		"sudo -n civmctl maintenance exit --execute",
	} {
		if !strings.Contains(body, cmd) {
			t.Errorf("civm-vhdx-optimize.ps1 must invoke %q (root-owned maintenance.lock needs sudo)", cmd)
		}
	}

	// Guard against the un-sudo'd RemoteCommand form that broke it.
	for i, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "RemoteCommand 'civmctl maintenance") {
			t.Errorf("civm-vhdx-optimize.ps1:%d invokes civmctl maintenance without sudo: %s",
				i+1, strings.TrimSpace(line))
		}
	}
}

// The optimize script runs with ErrorActionPreference=Stop, but OpenSSH writes
// normal remote stderr (including civmctl's supervised-operation warning) to
// PowerShell's error stream. The wrapper must capture both streams and decide
// from the native exit code, otherwise it terminates SSH mid-drain and can leave
// runner services stopped without a maintenance state snapshot.
func TestOptimizeSshCapturesStderrWithoutThrowing(t *testing.T) {
	body := readWindowsScript(t, "civm-vhdx-optimize.ps1")
	for _, want := range []string{
		"$oldPreference = $ErrorActionPreference",
		"$ErrorActionPreference = 'Continue'",
		"$exitCode = $LASTEXITCODE",
		"$ErrorActionPreference = $oldPreference",
		"ExitCode = $exitCode",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("civm-vhdx-optimize.ps1 missing stderr-safe SSH capture %q", want)
		}
	}
}
