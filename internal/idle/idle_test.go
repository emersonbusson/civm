package idle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckIdleBusyUnknown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fn      func(context.Context) ([]Activity, error)
		want    Status
		wantErr string
	}{
		{
			name: "idle",
			fn:   func(context.Context) ([]Activity, error) { return nil, nil },
			want: StatusIdle,
		},
		{
			name: "busy",
			fn: func(context.Context) ([]Activity, error) {
				return []Activity{{PID: 10, Command: "Runner.Worker run"}}, nil
			},
			want: StatusBusy,
		},
		{
			name: "unknown",
			fn:   func(context.Context) ([]Activity, error) { return nil, errors.New("ps unavailable") },
			want: StatusUnknown, wantErr: "ps unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Check(context.Background(), Options{ActivityFn: tt.fn})
			if got.Status != tt.want {
				t.Fatalf("Status = %s, want %s", got.Status, tt.want)
			}
			if tt.wantErr != "" && !strings.Contains(got.Error, tt.wantErr) {
				t.Fatalf("Error = %q, want contains %q", got.Error, tt.wantErr)
			}
			if got.ExitCode != tt.want.ExitCode() {
				t.Fatalf("ExitCode = %d, want %d", got.ExitCode, tt.want.ExitCode())
			}
		})
	}
}

func TestCheckSecondProbeCatchesNewActivity(t *testing.T) {
	t.Parallel()
	calls := 0
	got := Check(context.Background(), Options{
		ProbeDelay: time.Nanosecond,
		ActivityFn: func(context.Context) ([]Activity, error) {
			calls++
			if calls == 1 {
				return nil, nil
			}
			return []Activity{{PID: 20, Command: "docker compose build"}}, nil
		},
	})
	if got.Status != StatusBusy {
		t.Fatalf("Status = %s, want busy", got.Status)
	}
}

func TestEnsure(t *testing.T) {
	t.Parallel()
	if err := Ensure(context.Background(), Options{
		ActivityFn: func(context.Context) ([]Activity, error) { return nil, nil },
	}, "runner mutation"); err != nil {
		t.Fatalf("idle Ensure err = %v", err)
	}
	err := Ensure(context.Background(), Options{
		ActivityFn: func(context.Context) ([]Activity, error) {
			return []Activity{{PID: 1, Command: "Runner.Worker run"}}, nil
		},
	}, "runner mutation")
	if err == nil || !strings.Contains(err.Error(), "runner mutation abortado") {
		t.Fatalf("busy Ensure err = %v", err)
	}
}

func TestParseActiveProcessesDetectsWorkersAndBuilds(t *testing.T) {
	t.Parallel()
	ps := `
 100 1 Sl Runner.Listener /home/emdev/actions-runner/bin/Runner.Listener run --startuptype service
 101 100 Sl Runner.Worker /home/emdev/actions-runner/bin/Runner.Worker run
 102 100 S bash /home/emdev/actions-runner/_work/repo/repo/build.sh
 103 1 S docker docker buildx build .
 104 1 S sleep sleep 10
`
	got := ParseActiveProcesses(ps, 999)
	if len(got) != 3 {
		t.Fatalf("len(active) = %d, want 3: %+v", len(got), got)
	}
}

func TestIsActiveBuildProcessIgnoresCivmReadOnlyCommands(t *testing.T) {
	t.Parallel()
	if IsActiveBuildProcess("civmctl", "/usr/local/bin/civmctl idle-check --json") {
		t.Fatalf("idle-check should not self-block")
	}
	if IsActiveBuildProcess("civmctl", "/usr/local/bin/civmctl cleanup --execute --work-dir=/home/emdev/actions-runner/_work") {
		t.Fatalf("cleanup command should not self-block on its own --work-dir arg")
	}
	if !IsActiveBuildProcess("buildctl", "buildctl build --frontend dockerfile.v0") {
		t.Fatalf("buildctl should be active")
	}
}

func TestFormatActivitiesLimitsOutput(t *testing.T) {
	t.Parallel()
	activities := []Activity{
		{PID: 1, Command: strings.Repeat("a", 100)},
		{PID: 2, Command: "b"},
		{PID: 3, Command: "c"},
		{PID: 4, Command: "d"},
	}
	got := FormatActivities(activities)
	for _, want := range []string{"pid=1", "pid=2", "pid=3", "+1 outro"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatActivities omitted %q: %s", want, got)
		}
	}
}

func TestRenderJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Result{Status: StatusBusy, ExitCode: 1, Activities: []Activity{{PID: 1, Command: "Runner.Worker"}}}
	if err := r.RenderJSON(&buf); err != nil {
		t.Fatalf("RenderJSON err = %v", err)
	}
	var parsed Result
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed.Status != StatusBusy || parsed.ExitCode != 1 {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestRenderHuman(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	Result{Status: StatusBusy, ExitCode: 1, Activities: []Activity{{PID: 1, Command: "Runner.Worker"}}}.Render(&buf)
	for _, want := range []string{"busy", "Runner.Worker"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("Render omitted %q: %s", want, buf.String())
		}
	}
}
