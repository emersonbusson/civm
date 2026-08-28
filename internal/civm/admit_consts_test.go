package civm

import "testing"

// TestAdmitConstants pins the SPECv3 ("Constantes") admission defaults and the
// invariants the doctor/bootstrap rely on: at least one heavy slot and a
// positive host RAM reserve. The slot paths must be absolute (flock targets).
func TestAdmitConstants(t *testing.T) {
	t.Parallel()
	if DefaultAdmitMaxHeavy != 2 {
		t.Fatalf("DefaultAdmitMaxHeavy = %d, want 2", DefaultAdmitMaxHeavy)
	}
	if DefaultAdmitHostReserveMB != 2048 {
		t.Fatalf("DefaultAdmitHostReserveMB = %d, want 2048", DefaultAdmitHostReserveMB)
	}
	if DefaultAdmitHeavyMaxMB != 0 {
		t.Fatalf("DefaultAdmitHeavyMaxMB = %d, want 0 (generous)", DefaultAdmitHeavyMaxMB)
	}
	if DefaultAdmitWaitMinutes != 30 {
		t.Fatalf("DefaultAdmitWaitMinutes = %d, want 30", DefaultAdmitWaitMinutes)
	}
	if DefaultAdmitSlotPathPrefix != "/run/civm/admit-heavy-" {
		t.Fatalf("DefaultAdmitSlotPathPrefix = %q", DefaultAdmitSlotPathPrefix)
	}
	if DefaultAdmitDockerSlotPath != "/run/civm/admit-docker.lock" {
		t.Fatalf("DefaultAdmitDockerSlotPath = %q", DefaultAdmitDockerSlotPath)
	}
}

// TestAdmitInvariants asserts the safety invariants SPECv3 requires: MaxHeavy
// >= 1 (at least one heavy slot exists) and HostReserveMB > 0 (the host/SO
// always keeps RAM). These guard the doctor's RAM-fit invariant.
func TestAdmitInvariants(t *testing.T) {
	t.Parallel()
	if DefaultAdmitMaxHeavy < 1 {
		t.Fatalf("invariant MaxHeavy>=1 violated: %d", DefaultAdmitMaxHeavy)
	}
	if DefaultAdmitHostReserveMB <= 0 {
		t.Fatalf("invariant HostReserveMB>0 violated: %d", DefaultAdmitHostReserveMB)
	}
}
