package diskwatchdog

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/cleanup"
)

func TestCheck_OK_BelowThreshold(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 * (1 << 30), 50 * (1 << 30), nil // 50% used
	}
	o.ThresholdPct = 80
	r := Check(context.Background(), o)
	if r.Decision != DecisionOK {
		t.Errorf("Decision = %v, want OK", r.Decision)
	}
	if r.UsedPct != 50 {
		t.Errorf("UsedPct = %d, want 50", r.UsedPct)
	}
	if r.ExitCode() != 0 {
		t.Errorf("Exit = %d, want 0", r.ExitCode())
	}
	if len(r.CleanupActions) != 0 {
		t.Errorf("cleanup nao deveria ter rodado abaixo do threshold")
	}
}

func TestCheck_TriggersAboveThreshold(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 * (1 << 30), 5 * (1 << 30), nil // 95% used
	}
	o.TmpDir = t.TempDir()
	o.WorkDir = t.TempDir()
	o.ThresholdPct = 80
	o.Execute = false // dry-run cleanup
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("0B (0%)\n"), nil // empty docker
	}
	r := Check(context.Background(), o)
	if r.Decision != DecisionCleanupTriggered {
		t.Errorf("Decision = %v, want CleanupTriggered", r.Decision)
	}
	if r.UsedPct != 95 {
		t.Errorf("UsedPct = %d, want 95", r.UsedPct)
	}
	if r.ExitCode() != 0 {
		t.Errorf("Exit = %d, want 0 (cleanup ok)", r.ExitCode())
	}
	if len(r.CleanupActions) == 0 {
		t.Errorf("CleanupActions vazio")
	}
}

func TestCheck_StatfsError(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		return 0, 0, errors.New("statfs explodiu")
	}
	r := Check(context.Background(), o)
	if r.Decision != DecisionError {
		t.Errorf("Decision = %v, want Error", r.Decision)
	}
	if r.ExitCode() != 2 {
		t.Errorf("Exit = %d, want 2", r.ExitCode())
	}
}

// TestCheck_ExecuteBusyReclaimsUnusedDockerAndDefersRest (issue #70): above the
// threshold with an active Runner.Worker, the watchdog must NOT fail-closed with
// exit 2. It reclaims unused docker space (safe by construction) and defers the
// privileged file cleanup + aggressive system prune to an idle tick — a benign
// deferral, exit 0. (Replaces the prior test that asserted DecisionError/exit 2
// and that RunFn must never be called — the opposite of the desired purpose.)
func TestCheck_ExecuteBusyReclaimsUnusedDockerAndDefersRest(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		// 70% used: above the trigger threshold but BELOW the emergency-bypass
		// level, so the busy-host deferral is still the expected behavior. At
		// civm.DefaultEmergencyBypassPct+ the safe reclaim now runs even while
		// busy (TestCheck_ExecuteBusyEmergencyRunsSafeReclaim).
		return 100 * (1 << 30), 30 * (1 << 30), nil
	}
	o.ThresholdPct = 60
	o.Execute = true
	o.ActivityFn = func(context.Context) ([]cleanup.Activity, error) {
		return []cleanup.Activity{{PID: 4321, Command: "/home/emdev/actions-runner/bin/Runner.Worker run"}}, nil
	}
	var ranSafePrune, sawSystemPrune bool
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "image prune"):
			ranSafePrune = true
			return []byte("Total reclaimed space: 4GB\n"), nil
		case strings.Contains(joined, "builder prune"):
			return []byte("Total:  1GB\n"), nil
		case strings.Contains(joined, "system prune"):
			sawSystemPrune = true
		}
		return nil, nil
	}
	r := Check(context.Background(), o)
	if r.Decision != DecisionCleanupTriggered {
		t.Fatalf("Decision = %v, want CleanupTriggered (busy is a benign deferral)", r.Decision)
	}
	if r.ExitCode() != 0 {
		t.Fatalf("Exit = %d, want 0", r.ExitCode())
	}
	if r.Err != nil {
		t.Fatalf("Err = %v, want nil (host-busy is not a failure)", r.Err)
	}
	if !ranSafePrune {
		t.Fatalf("safe docker prune did not run above threshold while busy")
	}
	if sawSystemPrune {
		t.Fatalf("aggressive system prune ran while host busy")
	}
}

// TestCheck_ExecuteBusyDockerPruneErrorSurfacesExit2 pairs the benign-deferral
// test above with its negative: a host-busy deferral is exit 0, but a REAL
// failure of the safe docker prune is still a genuine error -> exit 2. This
// proves exit-2 is reserved for real faults, not for the (benign) busy state.
func TestCheck_ExecuteBusyDockerPruneErrorSurfacesExit2(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		// 70% used: below the emergency-bypass level so this test keeps
		// exercising the original busy-deferral path with a real prune error.
		return 100 * (1 << 30), 30 * (1 << 30), nil
	}
	o.ThresholdPct = 60
	o.Execute = true
	o.ActivityFn = func(context.Context) ([]cleanup.Activity, error) {
		return []cleanup.Activity{{PID: 4321, Command: "/home/emdev/actions-runner/bin/Runner.Worker run"}}, nil
	}
	o.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(name+" "+strings.Join(args, " "), "image prune") {
			return nil, errors.New("docker daemon unreachable")
		}
		return nil, nil
	}
	r := Check(context.Background(), o)
	if r.Decision != DecisionError {
		t.Fatalf("Decision = %v, want Error (real prune failure)", r.Decision)
	}
	if r.ExitCode() != 2 {
		t.Fatalf("Exit = %d, want 2", r.ExitCode())
	}
}

func TestCheck_StatfsZeroTotal(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		return 0, 0, nil // weird empty filesystem
	}
	r := Check(context.Background(), o)
	if r.Decision != DecisionError {
		t.Errorf("Decision = %v, want Error", r.Decision)
	}
}

func TestCheck_DefaultsApplied(t *testing.T) {
	t.Parallel()
	o := Options{
		StatfsFn: func(string) (uint64, uint64, error) {
			return 100 * (1 << 30), 50 * (1 << 30), nil
		},
	}
	// Path empty + ThresholdPct 0 -> defaults applied
	r := Check(context.Background(), o)
	if r.Path != "/" {
		t.Errorf("Path default = %q, want /", r.Path)
	}
	if r.ThresholdPct != 60 {
		t.Errorf("ThresholdPct default = %d, want 60", r.ThresholdPct)
	}
}

func TestRender_Triggered(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 * (1 << 30), 5 * (1 << 30), nil
	}
	o.TmpDir = t.TempDir()
	o.WorkDir = t.TempDir()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, nil
	}
	r := Check(context.Background(), o)
	var buf bytes.Buffer
	r.Render(&buf)
	out := buf.String()
	for _, want := range []string{"Disk watchdog", "95", "Threshold", "cleanup-triggered", "Aggressive cleanup"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render omitiu %q", want)
		}
	}
}

func TestRender_OK(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 * (1 << 30), 50 * (1 << 30), nil
	}
	r := Check(context.Background(), o)
	var buf bytes.Buffer
	r.Render(&buf)
	if !strings.Contains(buf.String(), "ok") {
		t.Errorf("Render(OK) sem 'ok'")
	}
	if strings.Contains(buf.String(), "Aggressive") {
		t.Errorf("Render(OK) com Aggressive section (cleanup nao deveria ter rodado)")
	}
}

func TestRender_Error(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	o.StatfsFn = func(string) (uint64, uint64, error) {
		return 0, 0, errors.New("disk on fire")
	}
	r := Check(context.Background(), o)
	var buf bytes.Buffer
	r.Render(&buf)
	if !strings.Contains(buf.String(), "disk on fire") {
		t.Errorf("Render(Error) sem mensagem de erro")
	}
}

func TestDecisionString(t *testing.T) {
	t.Parallel()
	cases := map[Decision]string{
		DecisionOK:               "ok",
		DecisionCleanupTriggered: "cleanup-triggered",
		DecisionError:            "error",
		Decision(99):             "?",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("Decision(%d).String() = %q, want %q", d, got, want)
		}
	}
}

func TestExitCode_AllDecisions(t *testing.T) {
	t.Parallel()
	cases := map[Decision]int{
		DecisionOK:               0,
		DecisionCleanupTriggered: 0,
		DecisionError:            2,
		Decision(99):             1,
	}
	for d, want := range cases {
		r := Result{Decision: d}
		if got := r.ExitCode(); got != want {
			t.Errorf("Decision(%d) Exit = %d, want %d", d, got, want)
		}
	}
}

func TestCheck_DefaultStatfsRealFS(t *testing.T) {
	// Não usa Parallel — toca FS real
	o := DefaultOptions()
	r := Check(context.Background(), o)
	if r.Decision == DecisionError {
		t.Skipf("statfs real falhou em ambiente: %v", r.Err)
	}
	if r.TotalGB == 0 {
		t.Errorf("Total = 0 GB; ambiente Linux normal deveria ter espaço")
	}
}
