//go:build integration

package safedelete

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/emersonbusson/civm/internal/civm"
)

// TestIntegrationEscalatesRealRootOwnedTarget is the gate that the #59 unit
// suite lacked: it exercises the ownership decision against a REAL root-owned
// directory on a REAL filesystem, not an injected FileOwnerUIDFn. A CI Docker
// step that runs as root leaves exactly this — a root-owned entry under _work —
// and the entire reason safedelete exists is to remove it. The original code
// refused such a target (uid != runner) and a hermetic unit test asserted that
// refusal as correct, locking in a wrong assumption (DT-v2-20).
//
// This test uses the real os.Lstat / owner extraction (defaults), so it FAILS
// on the refusing code: resolveAndAffirmOwner returns ErrUnsafePath and the
// escalation is never attempted. The privileged wrapper exec itself is stubbed
// (RunFn recorder) because asserting the ownership decision does not require
// real sudo; the full sudo+wrapper+delete chain is covered by the pre-deploy
// functional validation on the box (docs/specs/civm-runner-reliability/SPECv2.md
// §DT-v2-20).
//
// Requires: a non-root user with passwordless sudo to plant the root-owned
// fixture. It skips (never fails) when that environment is absent, so it is a
// no-op in unprivileged CI and a real gate on the self-hosted runner.
func TestIntegrationEscalatesRealRootOwnedTarget(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("must run as a non-root user to exercise the EACCES escalation path")
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		t.Skip("sudo not available; cannot plant a root-owned fixture")
	}

	// Canonicalize the base so EvalSymlinks(target) == target and the resolved
	// path never re-triggers the symlink GuardFn re-check.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(tempdir): %v", err)
	}
	target := filepath.Join(base, "rootscratch")
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}

	// Hand the whole subtree to root so the runner user cannot unlink it — the
	// exact Docker-as-root leftover. Passwordless sudo is required; without it we
	// cannot reproduce the condition, so skip rather than fail.
	if out, err := exec.Command("sudo", "-n", "chown", "-R", "root:root", target).CombinedOutput(); err != nil {
		t.Skipf("cannot chown root (no passwordless sudo?): %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("sudo", "-n", "rm", "-rf", target).Run() })

	// Confirm the fixture really is root-owned before we trust the assertion.
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat fixture: %v", err)
	}
	if uid, ok := defaultFileOwnerUID(info); !ok || uid != rootUID {
		t.Skipf("fixture is not root-owned (uid ok=%v); sudo chown had no effect", ok)
	}

	rec := &recorder{}
	opts := Options{
		// Scope the escalation to exactly this fixture; everything else (Lstat,
		// owner extraction, EvalSymlinks, OwnerUIDFn, RemoveAllFn) uses the real
		// os defaults via applyDefaults.
		GuardFn: func(p string) error {
			if p == target {
				return nil
			}
			return fmt.Errorf("unexpected path: %s", p)
		},
		RunFn: rec.run, // stub the privileged exec; record the escalation attempt
	}

	res := Remove(context.Background(), opts, target)

	if errors.Is(res.Err, ErrUnsafePath) {
		t.Fatalf("refused a REAL root-owned target: %v — the ownership guard is "+
			"blocking the tool's own purpose (this is the #59 regression)", res.Err)
	}
	if !res.Escalated {
		t.Fatalf("did not escalate a real root-owned target; Result=%+v", res)
	}
	ops := rec.ops()
	if len(ops) == 0 || ops[0] != "chown" {
		t.Fatalf("escalation ops = %v, want chown first (real root-owned target "+
			"must reach the privileged chown)", ops)
	}
}

// TestIntegrationRealWrapperDeletesRootOwnedFixture closes audit CRITICAL #1 and
// is the pre-deploy functional validation the user mandated (SPECv2 §DT-v2-20,
// option 3): it runs the ENTIRE privileged chain for real — `sudo -n
// civm-safedelete chown … / rm …` — against a real root-owned fixture under a
// real _work-prefix path, and asserts the tree is actually GONE. The prior test
// stubs RunFn (it proves the ownership decision); this one proves the sudoers
// rule matches, the wrapper is invokable as root, and the chown+rm really
// execute. An existence probe (`--check`) can never stand in for this.
//
// Requires: the runner user with the wrapper installed at
// civm.DefaultSafeDeleteWrapperPath, its NOPASSWD sudoers rule, and passwordless
// sudo to plant the root-owned fixture. Self-skips when any is absent, so it is a
// real gate only on a provisioned self-hosted runner.
func TestIntegrationRealWrapperDeletesRootOwnedFixture(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("must run as the non-root runner user")
	}
	wrapper := civm.DefaultSafeDeleteWrapperPath
	if _, err := os.Stat(wrapper); err != nil {
		t.Skipf("wrapper not installed at %s: %v", wrapper, err)
	}
	// Capability probe: the NOPASSWD rule must actually match. If it does not,
	// the whole chain cannot run — skip rather than fail (unprovisioned box).
	if err := exec.Command("sudo", "-n", wrapper, "--check").Run(); err != nil {
		t.Skipf("no passwordless capability for %s: %v", wrapper, err)
	}

	// The wrapper only accepts paths under a real runner _work tree
	// (/home/*/actions-runner*/_work/...). Build a dedicated scratch root that
	// matches the glob but never collides with a live runner's _work.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	scratchRoot := filepath.Join(home, "actions-runner-civm-integration", "_work")
	target := filepath.Join(scratchRoot, "rootscratch")
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort: hand ownership back, then remove the scratch tree.
		_ = exec.Command("sudo", "-n", "chown", "-R",
			fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			filepath.Join(home, "actions-runner-civm-integration")).Run()
		_ = os.RemoveAll(filepath.Join(home, "actions-runner-civm-integration"))
	})

	// Plant the root-owned condition. Passwordless sudo for a bare chown is a
	// separate capability from the wrapper NOPASSWD rule; skip if unavailable.
	if out, err := exec.Command("sudo", "-n", "chown", "-R", "root:root", target).CombinedOutput(); err != nil {
		t.Skipf("cannot plant root-owned fixture (no passwordless chown): %v: %s", err, out)
	}

	res := Remove(context.Background(), Options{
		GuardFn: func(p string) error {
			if p == target {
				return nil
			}
			return fmt.Errorf("unexpected path: %s", p)
		},
		// No RunFn / RemoveAllFn overrides: applyDefaults wires the REAL sudo exec
		// and os.RemoveAll, so this exercises the true privileged chain.
	}, target)

	if res.Err != nil {
		t.Fatalf("real-wrapper escalation failed: %v (Escalated=%v)", res.Err, res.Escalated)
	}
	if !res.Escalated {
		t.Fatalf("expected escalation for a real root-owned target; Result=%+v", res)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target still present after privileged delete (stat err=%v): the "+
			"sudo→wrapper→rm chain did not remove the root-owned tree", err)
	}
}
