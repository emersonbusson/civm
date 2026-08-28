package cleanup

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func busyActivity(context.Context) ([]Activity, error) {
	return []Activity{{PID: 4242, Command: "/home/emdev/actions-runner/bin/Runner.Worker run"}}, nil
}

// TestRunBusyEmergencyBypassRunsSafeReclaim guards the 2026-06-10 failure mode:
// the disk-watchdog fired at 83% used while a job filled the disk, every action
// was deferred-by-host-busy (freed=0) and the guest ran to 0% free, wedging
// sshd. Under EmergencyBypassIdle the SAFE reclaim (old /tmp, cache trim) must
// run even while busy; the privileged work-dir sweep must stay deferred.
func TestRunBusyEmergencyBypassRunsSafeReclaim(t *testing.T) {
	t.Parallel()
	opts := testExecuteOptions()
	opts.WorkDir = "work"
	opts.TmpDir = "tmp"
	opts.DockerPrune = true
	opts.AptClean = true
	opts.ActivityFn = busyActivity
	opts.EmergencyBypassIdle = true
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	// Hermetic cache trim: one fake cache root, no real filesystem touched.
	opts.GlobFn = func(pattern string) ([]string, error) { return nil, nil }
	rec := &safeDeleteRecorder{}
	opts.SafeDeleteFn = rec.fn

	actions := Run(context.Background(), opts)

	names := make([]string, 0, len(actions))
	for _, a := range actions {
		names = append(names, a.Name)
		if a.Err != nil {
			t.Fatalf("emergency bypass surfaced an error action: %+v", a)
		}
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "tmp_old") {
		t.Fatalf("emergency bypass must reclaim old /tmp while busy; actions=%s", joined)
	}
	if !strings.Contains(joined, "emergency-bypass-idle") {
		t.Fatalf("emergency bypass must be visible as an action; actions=%s", joined)
	}
	if strings.Contains(joined, "work_old") {
		t.Fatalf("privileged work sweep must stay deferred while busy; actions=%s", joined)
	}
	if strings.Contains(joined, deferredByHostBusy) {
		t.Fatalf("emergency bypass must replace the busy deferral; actions=%s", joined)
	}
}

func TestRunBusyEmergencyWipesColdWholeCaches(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 23, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	mfs := fstest.MapFS{
		"home/emdev/actions-runner/_work/repo": {Data: []byte("x"), ModTime: old},
		"home/emdev/.cache/go-build/aa/old-a":  {Data: []byte("cache"), ModTime: old},
	}
	var removed []string
	opts := testExecuteOptions()
	opts.Now = now
	opts.WorkDir = "home/emdev/actions-runner/_work"
	opts.TmpDir = "tmp"
	opts.DockerPrune = false
	opts.AptClean = false
	opts.ActivityFn = busyActivity
	opts.EmergencyBypassIdle = true
	opts.WalkFn = walkFS(mfs)
	opts.StatFn = func(p string) (fs.FileInfo, error) { return fs.Stat(mfs, p) }
	opts.GlobFn = func(pattern string) ([]string, error) { return fs.Glob(mfs, pattern) }
	opts.RemoveAllFn = func(path string) error {
		removed = append(removed, path)
		return nil
	}

	actions := Run(context.Background(), opts)

	var found Action
	for _, a := range actions {
		if a.Name == "cache_trim" && a.Path == "home/emdev/.cache/go-build" {
			found = a
			break
		}
	}
	if found.Path == "" {
		t.Fatalf("missing go-build cache_trim action: %+v", actions)
	}
	if found.BytesFreed == 0 {
		t.Fatalf("emergency go-build trim freed 0 bytes: %+v", found)
	}
	if strings.Join(removed, ",") != "home/emdev/.cache/go-build" {
		t.Fatalf("removed = %v, want whole go-build dir", removed)
	}
}

func TestRunBusyEmergencyPreservesInFlightWholeCaches(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 23, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Minute)
	mfs := fstest.MapFS{
		"home/emdev/actions-runner/_work/repo": {Data: []byte("x"), ModTime: fresh},
		"home/emdev/.cache/go-build/aa/live-a": {Data: []byte("cache"), ModTime: fresh},
	}
	var removed []string
	opts := testExecuteOptions()
	opts.Now = now
	opts.WorkDir = "home/emdev/actions-runner/_work"
	opts.TmpDir = "tmp"
	opts.DockerPrune = false
	opts.AptClean = false
	opts.ActivityFn = busyActivity
	opts.EmergencyBypassIdle = true
	opts.WalkFn = walkFS(mfs)
	opts.StatFn = func(p string) (fs.FileInfo, error) { return fs.Stat(mfs, p) }
	opts.GlobFn = func(pattern string) ([]string, error) { return fs.Glob(mfs, pattern) }
	opts.RemoveAllFn = func(path string) error {
		removed = append(removed, path)
		return nil
	}

	actions := Run(context.Background(), opts)

	for _, a := range actions {
		if a.Name == "cache_trim" && strings.HasPrefix(a.Path, "home/emdev/.cache/go-build") {
			if !strings.Contains(a.Path, "skipped: in-flight install") {
				t.Fatalf("fresh go-build cache must be skipped, got %+v", a)
			}
		}
	}
	if len(removed) != 0 {
		t.Fatalf("in-flight cache removed: %v", removed)
	}
}

// TestRunBusyWithoutEmergencyStillDefers is the refusal pair: below the
// emergency level the busy-host deferral is unchanged.
func TestRunBusyWithoutEmergencyStillDefers(t *testing.T) {
	t.Parallel()
	opts := testExecuteOptions()
	opts.WorkDir = "work"
	opts.TmpDir = "tmp"
	opts.ActivityFn = busyActivity
	opts.EmergencyBypassIdle = false
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }

	actions := Run(context.Background(), opts)

	var deferred bool
	for _, a := range actions {
		if a.Name == deferredByHostBusy {
			deferred = true
		}
		if a.Name == "tmp_old" || a.Name == "cache_trim" {
			t.Fatalf("non-emergency busy run must defer reclaim: %+v", actions)
		}
	}
	if !deferred {
		t.Fatalf("busy host without emergency must emit %s: %+v", deferredByHostBusy, actions)
	}
}
