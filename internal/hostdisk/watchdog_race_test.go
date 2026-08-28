package hostdisk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The civm-vhdx-optimize-watchdog re-starts the runner VM whenever it finds it
// Off, so a crashed reclaim that left the VM down recovers within 5 minutes.
// But it must NOT race Start-VM against a LIVE reclaim: both civm-vhdx-optimize
// and civm-vhdx-autoreclaim power the VM off to run Optimize-VHD and restart it
// in their own finally. Starting the VM mid-Optimize hits
// 0x80070020 ("arquivo ja esta sendo usado por outro processo") and was logged
// CRITICAL every 5 minutes during a reclaim window.
//
// The watchdog originally guarded only on the civm-vhdx-optimize Scheduled Task
// state, which is blind to civm-vhdx-autoreclaim (the task that actually fires
// every cycle). It must instead consult BOTH maintenance locks — those are held
// FileShare::None for the whole VM-Off window — before it ever calls Start-VM.
//
// This guard lints the generated watchdog body inside the registrar so the
// single-task regression can never return.
func TestOptimizeWatchdogConsultsBothMaintenanceLocks(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "windows", "register-civm-vhdx-optimize.ps1")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	base := filepath.Base(path)

	// Scope every assertion to the GENERATED watchdog body (the here-string),
	// not the registrar's own prose — the docstrings mention Start-VM and would
	// give false signals about ordering.
	const marker = "$watchdogBody = @\""
	_, rest, ok := strings.Cut(string(data), marker)
	if !ok {
		t.Fatalf("%s: watchdog here-string (%q) not found", base, marker)
	}
	wd, _, ok := strings.Cut(rest, "\"@")
	if !ok {
		t.Fatalf("%s: watchdog here-string not terminated", base)
	}

	// Both reclaim scripts must be honored: civm-vhdx-optimize uses
	// civm-optimize.lock, civm-vhdx-autoreclaim uses civm-autoreclaim.lock.
	for _, lock := range []string{"civm-optimize.lock", "civm-autoreclaim.lock"} {
		if !strings.Contains(wd, lock) {
			t.Errorf("%s watchdog must consult %s before Start-VM to avoid the "+
				"0x80070020 race; lock path not found", base, lock)
		}
	}

	// The actual call is `Start-VM -Name`; bare "Start-VM" also appears in the
	// watchdog's own docstring, so match the call form for the ordering check.
	callIdx := strings.Index(wd, "Start-VM -Name")
	if callIdx < 0 {
		t.Fatalf("%s: no `Start-VM -Name` call found in watchdog body", base)
	}

	// The maintenance-active guard must precede the Start-VM call, otherwise the
	// lock check is decorative and the race survives.
	autoreclaimIdx := strings.Index(wd, "civm-autoreclaim.lock")
	if autoreclaimIdx < 0 || autoreclaimIdx > callIdx {
		t.Errorf("%s: the civm-autoreclaim.lock guard must appear before the "+
			"Start-VM call (guard idx=%d, call idx=%d)", base, autoreclaimIdx, callIdx)
	}

	// A Start-VM that fails because a reclaim still holds the VHDX is transient,
	// not CRITICAL — a later 5-min tick recovers once the lock releases.
	if !strings.Contains(wd, "watchdog_start_vm_skipped_busy") {
		t.Errorf("%s: watchdog must downgrade a file-in-use Start-VM failure to a "+
			"transient skip (watchdog_start_vm_skipped_busy), not CRITICAL", base)
	}
}
