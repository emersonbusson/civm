package main

import (
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/admit"
)

func TestAdmitResolveWeight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		flag string
		env  string
		want admit.Weight
		err  bool
	}{
		{"explicit heavy", "heavy", "", admit.WeightHeavy, false},
		{"explicit light", "light", "", admit.WeightLight, false},
		{"auto falls back to env light", "auto", "light", admit.WeightLight, false},
		{"auto falls back to env heavy", "auto", "heavy", admit.WeightHeavy, false},
		{"auto with no env defaults heavy", "auto", "", admit.WeightHeavy, false},
		{"auto with junk env defaults heavy", "auto", "garbage", admit.WeightHeavy, false},
		{"invalid flag errors", "medium", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveWeight(c.flag, c.env)
			if c.err {
				if err == nil {
					t.Fatalf("resolveWeight(%q,%q) err = nil, want error", c.flag, c.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWeight(%q,%q) err = %v", c.flag, c.env, err)
			}
			if got != c.want {
				t.Fatalf("resolveWeight(%q,%q) = %q, want %q", c.flag, c.env, got, c.want)
			}
		})
	}
}

func TestAdmitEffectiveMemMB(t *testing.T) {
	t.Parallel()
	// HeavyMaxMB > 0 wins verbatim (calibrated cap, DT-v3-5).
	if got, err := effectiveMemMB(5000, 16384, 2048, 2); err != nil || got != 5000 {
		t.Fatalf("effectiveMemMB(calibrated) = %d, %v; want 5000, nil", got, err)
	}
	// HeavyMaxMB == 0 → generous (MemTotal - host)/MaxHeavy = (16384-2048)/2 = 7168.
	if got, err := effectiveMemMB(0, 16384, 2048, 2); err != nil || got != 7168 {
		t.Fatalf("effectiveMemMB(generous) = %d, %v; want 7168, nil", got, err)
	}
	// Unreadable host total (0) FAILS CLOSED — never admit a generous cap on a box
	// we couldn't measure (H3).
	if got, err := effectiveMemMB(0, 0, 2048, 2); err == nil {
		t.Fatalf("effectiveMemMB(memTotal=0) = %d, nil; want fail-closed error", got)
	}
	// Host too small for MaxHeavy (floor would overcommit) fails closed (H3).
	if got, err := effectiveMemMB(0, 1024, 2048, 2); err == nil {
		t.Fatalf("effectiveMemMB(degenerate) = %d, nil; want fail-closed error", got)
	}
}

func TestAdmitBuildSystemdRunArgs(t *testing.T) {
	t.Parallel()
	args := buildSystemdRunArgs(systemdRunSpec{
		User:  "emdev",
		Group: "emdev",
		MemMB: 7168,
		Unit:  "civm-admit-heavy-1-123.service",
		Cmd:   []string{"make", "up-local"},
	})
	joined := strings.Join(args, " ")
	// DT-v3-1: service transient --pipe --wait, NOT --scope; runs as emdev;
	// MemoryMax enforced; swap disabled. --collect garbage-collects the finished
	// unit so completed jobs never linger as "failed". --unit pins a deterministic
	// name so the slot record (written before start) can co-terminate it (DT-v3-2).
	mustContainSeq(t, args, "sudo", "systemd-run", "--collect", "--pipe", "--wait")
	mustContain(t, joined, "--unit=civm-admit-heavy-1-123.service")
	if strings.Contains(joined, "--scope") {
		t.Fatalf("systemd-run args must NOT use --scope (runs as root): %q", joined)
	}
	mustContain(t, joined, "-p User=emdev")
	mustContain(t, joined, "-p Group=emdev")
	mustContain(t, joined, "-p MemoryMax=7168M")
	mustContain(t, joined, "-p MemorySwapMax=0")
	// The payload must come after the -- separator, in order.
	sepIdx := indexOf(args, "--")
	if sepIdx < 0 || sepIdx >= len(args)-2 {
		t.Fatalf("missing -- separator before payload: %q", joined)
	}
	if args[sepIdx+1] != "make" || args[sepIdx+2] != "up-local" {
		t.Fatalf("payload not appended after --: %q", joined)
	}
}

func TestAdmitBuildSystemdRunArgsLightHasNoUnit(t *testing.T) {
	t.Parallel()
	// A light admission reserves no unit name, so no --unit flag is emitted and
	// systemd-run auto-names the (un-gated) transient unit.
	args := buildSystemdRunArgs(systemdRunSpec{User: "emdev", Group: "emdev", MemMB: 512, Cmd: []string{"true"}})
	if strings.Contains(strings.Join(args, " "), "--unit=") {
		t.Fatalf("light admission must not pin a --unit: %q", strings.Join(args, " "))
	}
}

func TestRunAdmitMissingExecIsUsageError(t *testing.T) {
	t.Parallel()
	// No `-- <cmd>` payload → exitUsage (DT: payload required).
	if code := runAdmit([]string{"--weight", "heavy"}); code != exitUsage {
		t.Fatalf("runAdmit(no payload) = %d, want %d", code, exitUsage)
	}
}

func TestRunAdmitBadFlagIsUsageError(t *testing.T) {
	t.Parallel()
	if code := runAdmit([]string{"--nope"}); code != exitUsage {
		t.Fatalf("runAdmit(bad flag) = %d, want %d", code, exitUsage)
	}
}

func TestRunAdmitInvalidWeightIsUsageError(t *testing.T) {
	t.Parallel()
	if code := runAdmit([]string{"--weight", "medium", "--exec", "--", "true"}); code != exitUsage {
		t.Fatalf("runAdmit(bad weight) = %d, want %d", code, exitUsage)
	}
}

// --- helpers ---

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("missing %q in %q", needle, haystack)
	}
}

func mustContainSeq(t *testing.T, args []string, seq ...string) {
	t.Helper()
	joined := strings.Join(args, " ")
	for _, s := range seq {
		if !strings.Contains(joined, s) {
			t.Fatalf("missing %q in %q", s, joined)
		}
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
