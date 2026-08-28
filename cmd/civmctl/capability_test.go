package main

import "testing"

func TestRunCapabilityRejectsUnknownCapability(t *testing.T) {
	if code := runCapability([]string{"unknown"}); code != exitUsage {
		t.Fatalf("unknown capability code = %d, want %d", code, exitUsage)
	}
}

func TestGenerationCleanBoundaryCapabilityConstants(t *testing.T) {
	if civmGenerationCleanBoundaryCapability != "generation-clean-boundary" {
		t.Fatalf("capability = %q", civmGenerationCleanBoundaryCapability)
	}
	if civmGenerationCleanBoundaryMarker != "civm-generation-boundary/v1" {
		t.Fatalf("marker = %q", civmGenerationCleanBoundaryMarker)
	}
}
