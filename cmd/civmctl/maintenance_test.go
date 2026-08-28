package main

import "testing"

func TestRunMaintenanceRejectsStrictForce(t *testing.T) {
	if code := runMaintenance([]string{"enter", "--strict", "--force"}); code != exitUsage {
		t.Fatalf("strict force code = %d, want %d", code, exitUsage)
	}
}

func TestRunMaintenanceRejectsStrictExit(t *testing.T) {
	if code := runMaintenance([]string{"exit", "--strict"}); code != exitUsage {
		t.Fatalf("strict exit code = %d, want %d", code, exitUsage)
	}
}
