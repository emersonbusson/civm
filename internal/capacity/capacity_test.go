package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// missingPath is a path that is guaranteed not to exist, used to drive
// error branches in syscall-backed defaults.
const missingPath = "/this/path/does/not/exist/civm-capacity-test"

func TestCheckBlocksWhenDiskHigh(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxDiskPct = 80
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 10, nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return []byte("0"), nil }
	r := Check(context.Background(), opts)
	if r.AcceptingJobs || r.DiskUsedPct != 90 {
		t.Fatalf("report=%+v", r)
	}
}

func TestCheckAcceptsWhenDiskOK(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxDiskPct = 85
	opts.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 << 30, 60 << 30, nil // 100GB total, 60GB free → 40% used
	}
	calls := map[string]int{}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls[name]++
		switch name {
		case "systemctl":
			return []byte("actions.runner.foo.service loaded\nactions.runner.bar.service loaded\n"), nil
		case "pgrep":
			return []byte("3\n"), nil
		}
		return nil, nil
	}
	r := Check(context.Background(), opts)
	if !r.AcceptingJobs {
		t.Fatalf("expected accepting, got %+v", r)
	}
	if r.DiskUsedPct != 40 || r.DiskFreeGB != 60 || r.DiskTotalGB != 100 {
		t.Fatalf("disk fields = %+v", r)
	}
	if r.RunnerServices != 2 || r.RunnerWorkers != 3 {
		t.Fatalf("runners = %+v", r)
	}
}

func TestCheckBlocksWhenStatfsFails(t *testing.T) {
	opts := DefaultOptions()
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 0, 0, errors.New("EIO") }
	r := Check(context.Background(), opts)
	if r.AcceptingJobs || r.Reason == "" {
		t.Fatalf("expected blocked with reason: %+v", r)
	}
}

func TestCheckBlocksWhenTotalZero(t *testing.T) {
	opts := DefaultOptions()
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 0, 0, nil }
	r := Check(context.Background(), opts)
	if r.AcceptingJobs {
		t.Fatalf("expected blocked when total=0: %+v", r)
	}
}

func TestCheckBlocksWhenRunnerQueueStalled(t *testing.T) {
	opts := DefaultOptions()
	opts.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 << 30, 60 << 30, nil
	}
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.QueueStallFn = func() (bool, string, error) {
		return true, "actions.runner.acme.civm-acme-org.service", nil
	}

	report := Check(context.Background(), opts)
	if report.AcceptingJobs || !report.QueueStalled ||
		!strings.Contains(report.Reason, "queue stall") {
		t.Fatalf("queue stall should block capacity: %+v", report)
	}
}

func TestCheckFailsClosedWhenQueueStateIsInvalid(t *testing.T) {
	opts := DefaultOptions()
	opts.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 << 30, 60 << 30, nil
	}
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.QueueStallFn = func() (bool, string, error) {
		return false, "", errors.New("invalid marker")
	}

	report := Check(context.Background(), opts)
	if report.AcceptingJobs || !strings.Contains(report.Reason, "queue state") {
		t.Fatalf("invalid queue state should fail closed: %+v", report)
	}
}

func TestQueueStallFromFileClassifiesMarkerStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		content     string
		writeFile   bool
		wantStalled bool
		wantUnit    string
		wantErr     bool
	}{
		{
			name:      "missing marker",
			writeFile: false,
		},
		{
			name:      "empty marker",
			writeFile: true,
			content:   " \n",
		},
		{
			name:      "malformed marker",
			writeFile: true,
			content:   "{",
			wantErr:   true,
		},
		{
			name:      "marker without active signature",
			writeFile: true,
			content:   `{"queue_stalls":{"runner-b":{"signature":" "}}}`,
		},
		{
			name:        "active incidents return deterministic unit",
			writeFile:   true,
			content:     `{"queue_stalls":{"runner-b":{"signature":"sig-b"},"runner-a":{"signature":"sig-a"}}}`,
			wantStalled: true,
			wantUnit:    "runner-a",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := t.TempDir() + "/runner-watchdog.json"
			if tt.writeFile {
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatalf("write marker: %v", err)
				}
			}

			stalled, unit, err := queueStallFromFile(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("queueStallFromFile() error = %v, wantErr=%v", err, tt.wantErr)
			}
			if stalled != tt.wantStalled || unit != tt.wantUnit {
				t.Fatalf(
					"queueStallFromFile() = (%v, %q), want (%v, %q)",
					stalled,
					unit,
					tt.wantStalled,
					tt.wantUnit,
				)
			}
		})
	}
}

func TestCheckAppliesDefaultsForEmptyOptions(t *testing.T) {
	r := Check(context.Background(), Options{
		StatfsFn: func(string) (uint64, uint64, error) { return 100, 50, nil },
		RunFn:    func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	})
	if r.DiskPath != "/" {
		t.Fatalf("Path default = %q", r.DiskPath)
	}
	if !r.AcceptingJobs {
		t.Fatalf("default hard-fail threshold should accept 50%% disk: %+v", r)
	}
}

func TestCountRunnerServicesOnRunFail(t *testing.T) {
	got := countRunnerServices(context.Background(),
		func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("boom") })
	if got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func TestCountRunnerWorkersHandlesInvalidOutput(t *testing.T) {
	got := countRunnerWorkers(context.Background(),
		func(context.Context, string, ...string) ([]byte, error) { return []byte("not-a-number"), nil })
	if got != 0 {
		t.Fatalf("got %d, want 0 for invalid pgrep output", got)
	}
}

func TestRenderJSONShape(t *testing.T) {
	var buf bytes.Buffer
	r := Report{DiskPath: "/", DiskUsedPct: 42, AcceptingJobs: true}
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatal(err)
	}
	var parsed Report
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v", err)
	}
	if parsed.DiskUsedPct != 42 || !parsed.AcceptingJobs {
		t.Fatalf("roundtrip: %+v", parsed)
	}
}

func TestRenderTextIncludesStateAndReason(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, Report{AcceptingJobs: false, Reason: "disk full"})
	out := buf.String()
	if !strings.Contains(out, "blocked") || !strings.Contains(out, "disk full") {
		t.Fatalf("text missing fields: %q", out)
	}
}

func TestCheckPopulatesDockerHeavyLockActive(t *testing.T) {
	r := Check(context.Background(), Options{
		StatfsFn:     func(string) (uint64, uint64, error) { return 100, 50, nil },
		RunFn:        func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		LockActiveFn: func() (bool, string, error) { return true, "docker-heavy acme/app#42", nil },
		PortBlocksFn: func() map[string]int { return map[string]int{"cmpx": 20000, "acme": 20064} },
	})
	if !r.DockerHeavyLockActive || r.DockerHeavyLockHolder != "docker-heavy acme/app#42" {
		t.Fatalf("lock fields not populated: %+v", r)
	}
	if r.RunnerPortBlocks["acme"] != 20064 {
		t.Fatalf("port blocks not populated: %+v", r.RunnerPortBlocks)
	}
	var buf bytes.Buffer
	RenderText(&buf, r)
	if !strings.Contains(buf.String(), "Docker-heavy lock: held by docker-heavy acme/app#42") {
		t.Fatalf("text missing lock holder: %q", buf.String())
	}
}

func TestCheckPopulatesAdmitStatus(t *testing.T) {
	r := Check(context.Background(), Options{
		StatfsFn: func(string) (uint64, uint64, error) { return 100, 50, nil },
		RunFn:    func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		AdmitStatusFn: func() AdmitStatus {
			return AdmitStatus{HeavyLive: 1, MaxHeavy: 2}
		},
	})
	if r.Admit == nil {
		t.Fatalf("Admit not populated")
	}
	if r.Admit.HeavyLive != 1 || r.Admit.MaxHeavy != 2 {
		t.Fatalf("admit status = %+v, want {1 2}", r.Admit)
	}
	var buf bytes.Buffer
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"admit"`) ||
		!strings.Contains(buf.String(), `"heavy_live": 1`) ||
		!strings.Contains(buf.String(), `"max_heavy": 2`) {
		t.Fatalf("admit JSON shape missing: %s", buf.String())
	}
}

func TestCountLiveHeavySlots(t *testing.T) {
	// Slots 1..2: slot 1 is "held" (flock fails => live), slot 2 free.
	held := map[string]bool{"/run/civm/admit-heavy-1.lock": true}
	n := countLiveHeavySlots("/run/civm/admit-heavy-", 2, func(path string) (bool, error) {
		return !held[path], nil // free => can flock
	})
	if n != 1 {
		t.Fatalf("countLiveHeavySlots = %d, want 1", n)
	}
	// All free → 0 live.
	if n := countLiveHeavySlots("/run/civm/admit-heavy-", 2, func(string) (bool, error) { return true, nil }); n != 0 {
		t.Fatalf("countLiveHeavySlots(all free) = %d, want 0", n)
	}
	// A probe error for a slot is counted conservatively as not-live (best-effort).
	if n := countLiveHeavySlots("/run/civm/admit-heavy-", 2, func(string) (bool, error) { return false, errors.New("io") }); n != 0 {
		t.Fatalf("countLiveHeavySlots(probe error) = %d, want 0", n)
	}
}

func TestCheckLockErrorLeavesInactive(t *testing.T) {
	r := Check(context.Background(), Options{
		StatfsFn:     func(string) (uint64, uint64, error) { return 100, 50, nil },
		RunFn:        func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		LockActiveFn: func() (bool, string, error) { return false, "", errors.New("read .hb failed") },
		PortBlocksFn: func() map[string]int { return nil },
	})
	if r.DockerHeavyLockActive || r.DockerHeavyLockHolder != "" {
		t.Fatalf("lock read error must leave inactive/empty: %+v", r)
	}
	if !r.AcceptingJobs {
		t.Fatalf("lock telemetry error must not block job acceptance: %+v", r)
	}
}

// TestCheckUsesDefaultFnsWhenNil drives Check with a fully empty Options so the
// nil-guards wire in defaultStatfs/defaultRun/defaultLockActive/defaultPortBlocks.
// The default StatfsFn reads the real "/" mount, which is always present in CI,
// so the call must succeed and the report must accept jobs (root is never full).
func TestCheckUsesDefaultFnsWhenNil(t *testing.T) {
	r := Check(context.Background(), Options{})
	if r.DiskPath != "/" {
		t.Fatalf("default path = %q, want /", r.DiskPath)
	}
	if r.DiskTotalGB <= 0 {
		t.Fatalf("default statfs should report a non-zero total: %+v", r)
	}
	// The host root is not expected to be full; defaults must accept jobs and
	// must not record a blocking reason.
	if !r.AcceptingJobs || r.Reason != "" {
		t.Fatalf("defaults should accept jobs on a healthy root: %+v", r)
	}
}

// TestCountRunnerWorkersOnRunFail covers the error branch where pgrep itself
// fails (distinct from the already-tested invalid-output branch).
func TestCountRunnerWorkersOnRunFail(t *testing.T) {
	got := countRunnerWorkers(context.Background(),
		func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("pgrep boom") })
	if got != 0 {
		t.Fatalf("got %d, want 0 when pgrep fails", got)
	}
}

// TestRenderTextLockHeldWithoutHolder covers the "(unknown)" fallback when the
// lock is active but the holder label is empty.
func TestRenderTextLockHeldWithoutHolder(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, Report{AcceptingJobs: true, DockerHeavyLockActive: true})
	out := buf.String()
	if !strings.Contains(out, "Docker-heavy lock: held by (unknown)") {
		t.Fatalf("expected unknown-holder fallback, got %q", out)
	}
}

// TestRenderTextNoLockOmitsLockLine asserts the lock line is absent when no lock
// is held, keeping the accepting/blocked branches symmetric.
func TestRenderTextNoLockOmitsLockLine(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, Report{AcceptingJobs: true})
	if strings.Contains(buf.String(), "Docker-heavy lock") {
		t.Fatalf("lock line should be omitted when inactive: %q", buf.String())
	}
}

// TestDefaultStatfs exercises the real syscall path for both the happy "/" case
// and the error branch for a path that cannot be stat'd.
func TestDefaultStatfs(t *testing.T) {
	total, free, err := defaultStatfs("/")
	if err != nil {
		t.Fatalf("statfs / failed: %v", err)
	}
	if total == 0 || free > total {
		t.Fatalf("implausible statfs result: total=%d free=%d", total, free)
	}

	if _, _, err := defaultStatfs(missingPath); err == nil {
		t.Fatalf("statfs on a missing path should error")
	}
}

// TestDefaultRun runs a trivial, always-present command to cover the production
// exec wrapper. "true" succeeds with empty output on every supported platform.
func TestDefaultRun(t *testing.T) {
	out, err := defaultRun(context.Background(), "true")
	if err != nil {
		t.Fatalf("defaultRun true failed: %v out=%q", err, out)
	}
}

// TestDefaultLockActiveAbsent calls the production default directly. In the unit
// environment the docker-heavy lock heartbeat file is absent, so dockerlock
// reports not-active and the wrapper returns the benign (false,"",nil) tuple.
func TestDefaultLockActiveAbsent(t *testing.T) {
	active, holder, err := defaultLockActive()
	if err != nil {
		t.Fatalf("defaultLockActive should not error when lock is absent: %v", err)
	}
	if active || holder != "" {
		t.Fatalf("absent lock must report inactive/empty, got active=%v holder=%q", active, holder)
	}
}

// TestDefaultPortBlocksAbsent calls the production default directly. The sticky
// port-block state file is absent in the unit environment, so the read error
// branch must yield a nil map (observability is best-effort, never fatal).
func TestDefaultPortBlocksAbsent(t *testing.T) {
	if got := defaultPortBlocks(); got != nil {
		t.Fatalf("absent port-block state must yield nil, got %+v", got)
	}
}
