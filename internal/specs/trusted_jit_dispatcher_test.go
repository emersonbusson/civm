package specs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustedJITDispatcherRetiresUnsafeLoopSource(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "deploy", "bin", "civm-ephemeral-runner.sh"),
		filepath.Join("..", "..", "deploy", "systemd", "civm-ephemeral-runner@.service"),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("retired unsafe JIT loop still exists at %s: %v", path, err)
		}
	}
}

func TestTrustedJITDispatcherIsRepositoryScopedAndHasNoRunDiscovery(t *testing.T) {
	githubPath := filepath.Join("..", "jitdispatcher", "github.go")
	githubData, err := os.ReadFile(githubPath)
	if err != nil {
		t.Fatal(err)
	}
	github := string(githubData)
	for _, required := range []string{
		`/actions/runners/generate-jitconfig`,
		`workflow_run_id`,
		`status != http.StatusOK`,
		`decodeUniqueJSON`,
	} {
		if !strings.Contains(github, required) {
			t.Fatalf("GitHub client is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`/orgs/`,
		`/actions/workflows/" + workflow + "/runs`,
		`docker system prune`,
		`--volumes`,
	} {
		if strings.Contains(github, forbidden) {
			t.Fatalf("GitHub client contains forbidden path or cleanup %q", forbidden)
		}
	}
}

func TestTrustedJITRunnerRequiresDisposableIsolationProof(t *testing.T) {
	runnerPath := filepath.Join("..", "jitdispatcher", "runner.go")
	runnerData, err := os.ReadFile(runnerPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := string(runnerData)
	for _, required := range []string{
		`"recover", "--protocol=1"`,
		"validReadyReceipt",
		"validDestroyedReceipt",
		"HostDocker",
		"ResetVerified",
		`exec.Command("/proc/self/fd/4"`,
	} {
		if !strings.Contains(runner, required) {
			t.Fatalf("runner lacks disposable-isolation control %q", required)
		}
	}
	for _, forbidden := range []string{
		"docker ", "system prune", "volume prune", "fstrim",
		"cleanupWorkFolder", `exec.Command(filepath.Join(request.RunnerDirectory, "run.sh")`,
	} {
		if strings.Contains(runner, forbidden) {
			t.Fatalf("runner cleanup contains forbidden operation %q", forbidden)
		}
	}
}

func TestTrustedJITRunnerIsBornInsideItsLeaseCgroup(t *testing.T) {
	containmentData, err := os.ReadFile(filepath.Join("..", "jitdispatcher", "containment_linux.go"))
	if err != nil {
		t.Fatal(err)
	}
	containment := string(containmentData)
	for _, required := range []string{
		"UseCgroupFD = true",
		"CgroupFD = int(file.Fd())",
		"verifyMembership := processInCgroup",
		"verifyMembership(pid, child)",
		"cgroup.kill",
	} {
		if !strings.Contains(containment, required) {
			t.Fatalf("pre-start cgroup containment is missing %q", required)
		}
	}
	if strings.Contains(containment, `os.WriteFile(filepath.Join(child, "cgroup.procs")`) {
		t.Fatal("runner is still attached to the cgroup only after process start")
	}
}

func TestTrustedJITAdmissionIsGuardOwnedAndMachineWide(t *testing.T) {
	typesData, err := os.ReadFile(filepath.Join("..", "jitdispatcher", "types.go"))
	if err != nil {
		t.Fatal(err)
	}
	types := string(typesData)
	for _, required := range []string{
		`MachineLockPath = "/run/civm/jit-dispatch.lock"`,
		`LeaseMarkerPath = "/run/civm/jit-dispatch-lease.json"`,
	} {
		if !strings.Contains(types, required) {
			t.Fatalf("fixed machine-wide path is missing %q", required)
		}
	}

	gateData, err := os.ReadFile(filepath.Join("..", "jitdispatcher", "gate_linux.go"))
	if err != nil {
		t.Fatal(err)
	}
	gate := string(gateData)
	for _, required := range []string{
		`guardExecutable, "exec", "--", gate.SelfExecutable`,
		`"__jit-lease-hold", "--lease-id", leaseID`,
		`"--admission-id", admission.Nonce`,
		`processIdentityAlive(marker.HolderPID, marker.StartTicks)`,
	} {
		if !strings.Contains(gate, required) {
			t.Fatalf("Guard-owned admission control is missing %q", required)
		}
	}
	if strings.Contains(gate, "state_dir") {
		t.Fatal("Guard admission is coupled to configurable state_dir")
	}
}

func TestTrustedJITHeadroomContractRejectsTheoreticalOvercommit(t *testing.T) {
	specData, err := os.ReadFile(filepath.Join("..", "..", "docs", "specs", "trusted-jit-dispatcher", "SPEC.md"))
	if err != nil {
		t.Fatal(err)
	}
	spec := string(specData)
	for _, required := range []string{
		"`31,90 GiB`", "`20 GiB`", "`12 GiB`", "`32 GiB`",
		"piso Windows", "margem de segurança", "leitura Windows válida após eventual reclaim",
	} {
		if !strings.Contains(spec, required) {
			t.Fatalf("headroom contract is missing %q", required)
		}
	}
}
