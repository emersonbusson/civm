//go:build linux

package jitdispatcher

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerPersistsReadyBeforeSecretAndRequiresDestruction(t *testing.T) {
	request, containment := isolatedRunnerFixture(t)
	ready := receiptJSON(t, IsolationReceipt{
		Protocol: 1, Event: "ready", LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-0001", BaseSHA256: request.BaseImageSHA256, Disposable: true,
	})
	destroyed := receiptJSON(t, IsolationReceipt{
		Protocol: 1, Event: "destroyed", LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-0001", BaseSHA256: request.BaseImageSHA256, Disposable: true,
		Destroyed: true, ResetVerified: true,
	})
	driver, digest := writeDriver(t, fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' '%s' >&3
IFS= read -r payload
printf 'received=%%s\n' "$payload"
printf '%%s\n' '%s' >&3
`, ready, destroyed))
	request.DriverExecutable = driver
	request.DriverSHA256 = digest
	var callbacks []string
	request.OnStarted = func(process ProcessIdentity) error {
		callbacks = append(callbacks, "started")
		if process.PID <= 1 || process.StartTicks == 0 {
			return errors.New("missing process identity")
		}
		return nil
	}
	request.OnReady = func(receipt IsolationReceipt) error {
		callbacks = append(callbacks, "ready")
		return nil
	}
	outcome, err := (ExecRunner{Containment: containment}).Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.Join(callbacks, ",") != "started,ready" || !outcome.Destroyed.ResetVerified {
		t.Fatalf("callbacks/outcome = %v / %+v", callbacks, outcome)
	}
	if containment.prepareCalls != 1 || containment.attachCalls != 1 || containment.terminateCalls != 1 {
		t.Fatalf("containment calls = prepare:%d attach:%d terminate:%d", containment.prepareCalls, containment.attachCalls, containment.terminateCalls)
	}
	logData, err := os.ReadFile(filepath.Join(request.LogDirectory, "isolation-driver.log"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(logData, request.JITConfig) || !bytes.Contains(logData, []byte("[REDACTED]")) {
		t.Fatalf("driver log did not redact JIT input: %s", logData)
	}
}

func TestExecRunnerNeverSendsSecretWhenReadyPersistenceFails(t *testing.T) {
	request, containment := isolatedRunnerFixture(t)
	marker := filepath.Join(t.TempDir(), "secret-received")
	ready := receiptJSON(t, IsolationReceipt{
		Protocol: 1, Event: "ready", LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-0002", BaseSHA256: request.BaseImageSHA256, Disposable: true,
	})
	driver, digest := writeDriver(t, fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' '%s' >&3
if IFS= read -r payload; then
  printf 'received' > '%s'
fi
`, ready, marker))
	request.DriverExecutable = driver
	request.DriverSHA256 = digest
	request.OnReady = func(IsolationReceipt) error { return errors.New("ledger unavailable") }
	_, err := (ExecRunner{Containment: containment}).Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "ledger unavailable") {
		t.Fatalf("Run() callback error = %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("JIT secret reached driver before ready persistence: %v", statErr)
	}
}

func TestExecRunnerRejectsUnsafeReadyReceiptBeforeSecret(t *testing.T) {
	request, containment := isolatedRunnerFixture(t)
	marker := filepath.Join(t.TempDir(), "secret-received")
	unsafeReady := receiptJSON(t, IsolationReceipt{
		Protocol: 1, Event: "ready", LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-0003", BaseSHA256: request.BaseImageSHA256,
		Disposable: true, HostDocker: true,
	})
	driver, digest := writeDriver(t, fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' '%s' >&3
if IFS= read -r payload; then
  printf 'received' > '%s'
fi
`, unsafeReady, marker))
	request.DriverExecutable = driver
	request.DriverSHA256 = digest
	_, err := (ExecRunner{Containment: containment}).Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "ready proof is invalid") {
		t.Fatalf("Run() unsafe receipt error = %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe isolation received JIT secret: %v", statErr)
	}
}

func TestExecRunnerRecoveryIsRepeatableAndFailClosed(t *testing.T) {
	request, containment := isolatedRunnerFixture(t)
	destroyed := receiptJSON(t, IsolationReceipt{
		Protocol: 1, Event: "destroyed", LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-0004", BaseSHA256: request.BaseImageSHA256, Disposable: true,
		Destroyed: true, ResetVerified: true,
	})
	driver, digest := writeDriver(t, fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s\\n' '%s' >&3\n", destroyed))
	recovery := RecoveryRequest{
		DriverExecutable: driver, DriverSHA256: digest, BaseImageSHA256: request.BaseImageSHA256,
		RunnerDirectory: request.RunnerDirectory, LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-0004", LogDirectory: request.LogDirectory,
		ShutdownGrace: request.ShutdownGrace,
	}
	runner := ExecRunner{Containment: containment}
	for attempt := 0; attempt < 2; attempt++ {
		receipt, err := runner.Recover(context.Background(), recovery)
		if err != nil || !receipt.ResetVerified {
			t.Fatalf("Recover() attempt %d = %+v, %v", attempt+1, receipt, err)
		}
	}
	if containment.attachCalls != 2 || containment.terminateCalls != 2 {
		t.Fatalf("repeat recovery containment calls = attach:%d terminate:%d", containment.attachCalls, containment.terminateCalls)
	}

	badDriver, badDigest := writeDriver(t, fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' '%s' >&3
`, receiptJSON(t, IsolationReceipt{
		Protocol: 1, Event: "destroyed", LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-0004", BaseSHA256: request.BaseImageSHA256, Disposable: true,
		Destroyed: true, ResetVerified: false,
	})))
	recovery.DriverExecutable = badDriver
	recovery.DriverSHA256 = badDigest
	if _, err := runner.Recover(context.Background(), recovery); err == nil {
		t.Fatal("Recover() accepted destruction without reset verification")
	}
}

func TestExecRunnerTimeoutTerminatesContainment(t *testing.T) {
	request, containment := isolatedRunnerFixture(t)
	containment.kill = true
	ready := receiptJSON(t, IsolationReceipt{
		Protocol: 1, Event: "ready", LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-0005", BaseSHA256: request.BaseImageSHA256, Disposable: true,
	})
	driver, digest := writeDriver(t, fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' '%s' >&3
IFS= read -r payload
/bin/sleep 5
`, ready))
	request.DriverExecutable = driver
	request.DriverSHA256 = digest
	request.ShutdownGrace = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (ExecRunner{Containment: containment}).Run(ctx, request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("containment termination took %s", elapsed)
	}
}

func TestExecRunnerReapsDriverWhenStartedPersistenceFails(t *testing.T) {
	request, containment := isolatedRunnerFixture(t)
	containment.kill = true
	driver, digest := writeDriver(t, "#!/bin/sh\nset -eu\n/bin/sleep 5\n")
	request.DriverExecutable = driver
	request.DriverSHA256 = digest
	request.OnStarted = func(ProcessIdentity) error { return errors.New("persist process identity") }
	started := time.Now()
	_, err := (ExecRunner{Containment: containment}).Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "persist process identity") {
		t.Fatalf("Run() persistence error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("driver was not reaped promptly: %s", elapsed)
	}
	if containment.terminateCalls != 1 {
		t.Fatalf("Terminate() calls = %d", containment.terminateCalls)
	}
}

func TestExecRunnerRejectsDigestDriftAndUnsafeInputs(t *testing.T) {
	request, containment := isolatedRunnerFixture(t)
	driver, digest := writeDriver(t, "#!/bin/sh\nexit 0\n")
	request.DriverExecutable = driver
	request.DriverSHA256 = digest
	if err := os.WriteFile(driver, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := (ExecRunner{Containment: containment}).Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Run() digest drift error = %v", err)
	}

	validDriver, validDigest := writeDriver(t, "#!/bin/sh\nexit 0\n")
	request.DriverExecutable = validDriver
	request.DriverSHA256 = validDigest
	tests := []RunnerRequest{
		func() RunnerRequest { value := request; value.Identity = Identity{}; return value }(),
		func() RunnerRequest { value := request; value.JITConfig = []byte("short"); return value }(),
		func() RunnerRequest {
			value := request
			value.JITConfig = []byte("encoded jit secret value")
			return value
		}(),
		func() RunnerRequest { value := request; value.ShutdownGrace = 0; return value }(),
	}
	for index, candidate := range tests {
		if _, err := (ExecRunner{Containment: containment}).Run(context.Background(), candidate); !errors.Is(err, ErrInvalid) {
			t.Errorf("unsafe case %d error = %v", index, err)
		}
	}
}

func TestExecRunnerPreflightPinsDriverAndRefusesWritableRunnerSource(t *testing.T) {
	request, containment := isolatedRunnerFixture(t)
	driver, digest := writeDriver(t, "#!/bin/sh\nexit 0\n")
	config := Config{
		IsolationDriver: driver, DriverSHA256: digest,
		RunnerDirectory: request.RunnerDirectory,
	}
	runner := ExecRunner{Containment: containment}
	if err := runner.Preflight(config); err != nil {
		t.Fatalf("Preflight() valid error = %v", err)
	}
	if err := os.Chmod(request.RunnerDirectory, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := runner.Preflight(config); err == nil {
		t.Fatal("Preflight() accepted group-writable runner source")
	}
}

func TestLineRedactorHandlesChunkedSecret(t *testing.T) {
	var output bytes.Buffer
	writer := newLineRedactor(&output, []byte("log: "), [][]byte{[]byte("chunked-secret")})
	for _, chunk := range []string{"value=chunk", "ed-se", "cret\nnext"} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "chunked-secret") || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("redacted output = %q", output.String())
	}
}

func TestRecoveryLogRefusesSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.log")
	if err := os.WriteFile(target, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "recovery.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if file, err := openLog(link, true); err == nil {
		_ = file.Close()
		t.Fatal("openLog() followed a recovery-log symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "preserve\n" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}

func TestReadReceiptFailsClosedOnMalformedOversizedAndCanceledInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
	}{
		{name: "malformed", input: "{not-json}\n"},
		{name: "duplicate key", input: `{"protocol":1,"protocol":2}` + "\n"},
		{name: "oversized", input: strings.Repeat("x", (64<<10)+1) + "\n"},
		{name: "truncated", input: `{"protocol":1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readReceipt(context.Background(), bufio.NewReader(strings.NewReader(test.input))); err == nil {
				t.Fatal("readReceipt() accepted unsafe control input")
			}
		})
	}

	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readReceipt(ctx, bufio.NewReader(reader)); !errors.Is(err, context.Canceled) {
		t.Fatalf("readReceipt() cancellation error = %v", err)
	}
	_ = writer.Close()
	_ = reader.Close()
}

func TestRunnerRecoveryValidationAndReapHelpers(t *testing.T) {
	runner := NewExecRunner()
	if runner.Containment == nil {
		t.Fatal("NewExecRunner() returned no containment")
	}
	if err := (ExecRunner{}).Preflight(Config{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Preflight() missing containment error = %v", err)
	}

	request, containment := isolatedRunnerFixture(t)
	recovery := RecoveryRequest{
		DriverSHA256: strings.Repeat("a", 64), BaseImageSHA256: request.BaseImageSHA256,
		RunnerDirectory: request.RunnerDirectory, LeaseID: request.LeaseID,
		IsolationID: "disposable-vm-validation", LogDirectory: request.LogDirectory,
		ShutdownGrace: request.ShutdownGrace,
	}
	if err := validateRecoveryRequest(recovery, containment); err != nil {
		t.Fatalf("validateRecoveryRequest() error = %v", err)
	}
	for _, mutate := range []func(*RecoveryRequest){
		func(value *RecoveryRequest) { value.IsolationID = "bad/id" },
		func(value *RecoveryRequest) { value.RunnerDirectory = "relative" },
		func(value *RecoveryRequest) { value.LogDirectory = "/" },
		func(value *RecoveryRequest) { value.ShutdownGrace = 0 },
	} {
		candidate := recovery
		mutate(&candidate)
		if err := validateRecoveryRequest(candidate, containment); !errors.Is(err, ErrInvalid) {
			t.Fatalf("validateRecoveryRequest() unsafe error = %v", err)
		}
	}

	wait := make(chan error, 1)
	wait <- nil
	close(wait)
	if err := stopUnidentifiedAndWait(wait, containment, request.LeaseID, time.Millisecond); err != nil {
		t.Fatalf("stopUnidentifiedAndWait() error = %v", err)
	}
	if err := waitForExit(nil, time.Millisecond); err != nil {
		t.Fatalf("waitForExit(nil) error = %v", err)
	}
	never := make(chan error)
	if err := waitForExit(never, time.Millisecond); err == nil {
		t.Fatal("waitForExit() accepted an unreaped process")
	}
}

func TestOpenLogModesAndRedactionLimits(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "driver.log")
	file, err := openLog(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := openLog(path, false); err == nil {
		_ = duplicate.Close()
		t.Fatal("openLog() replaced an existing run log")
	}
	appendFile, err := openLog(path, true)
	if err != nil {
		t.Fatalf("openLog() append error = %v", err)
	}
	_ = appendFile.Close()
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if unsafe, err := openLog(path, true); err == nil {
		_ = unsafe.Close()
		t.Fatal("openLog() accepted permissive existing log")
	}

	writer := newLineRedactor(io.Discard, nil, nil)
	if _, err := writer.Write(bytes.Repeat([]byte{'x'}, maxLogLine+1)); err == nil {
		t.Fatal("lineRedactor accepted an unbounded line")
	}
	redactions := normalizedRedactions([][]byte{
		[]byte("abc"), []byte("long-secret"), []byte("long-secret"), []byte("short-secret"),
	})
	if len(redactions) != 2 || string(redactions[0]) != "short-secret" || string(redactions[1]) != "long-secret" {
		t.Fatalf("normalizedRedactions() = %q", redactions)
	}
	zeroRedactions(redactions)
	for _, value := range redactions {
		if bytes.Count(value, []byte{0}) != len(value) {
			t.Fatalf("zeroRedactions() left data: %q", value)
		}
	}
}

type fakeContainment struct {
	prepareCalls   int
	attachCalls    int
	terminateCalls int
	kill           bool
}

func (fake *fakeContainment) Prepare(*exec.Cmd, string) (io.Closer, error) {
	fake.prepareCalls++
	return io.NopCloser(strings.NewReader("")), nil
}

func (fake *fakeContainment) Attach(pid int, leaseID string) (ProcessIdentity, error) {
	fake.attachCalls++
	return ProcessIdentity{
		PID: pid, StartTicks: uint64(pid) + 100, ProcessGroup: pid,
		CgroupPath: "/sys/fs/cgroup/civm-jit-" + leaseID,
	}, nil
}

func (fake *fakeContainment) Terminate(_ context.Context, identity ProcessIdentity, _ time.Duration) error {
	fake.terminateCalls++
	if fake.kill {
		return signalProcessGroup(identity.ProcessGroup, true)
	}
	return nil
}

func (fake *fakeContainment) Recover(_ context.Context, _ string, identity ProcessIdentity, _ time.Duration) error {
	if identity.PID == 0 {
		return nil
	}
	return fake.Terminate(context.Background(), identity, time.Millisecond)
}

func (*fakeContainment) Alive(ProcessIdentity) (bool, error) { return false, nil }

func isolatedRunnerFixture(t *testing.T) (RunnerRequest, *fakeContainment) {
	t.Helper()
	identity, err := NewIdentity(strings.NewReader(strings.Repeat("r", 32)))
	if err != nil {
		t.Fatal(err)
	}
	runnerDirectory := filepath.Join(t.TempDir(), "runner")
	logDirectory := filepath.Join(t.TempDir(), "logs")
	for _, directory := range []string{runnerDirectory, logDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return RunnerRequest{
		BaseImageSHA256: strings.Repeat("b", 64), RunnerDirectory: runnerDirectory,
		Identity: identity, LeaseID: identity.Nonce,
		JITConfig: []byte("encoded-jit-sensitive-value"), LogDirectory: logDirectory,
		ShutdownGrace: 50 * time.Millisecond,
	}, &fakeContainment{}
}

func writeDriver(t *testing.T, content string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "isolation-driver")
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	return path, hex.EncodeToString(digest[:])
}

func receiptJSON(t *testing.T, receipt IsolationReceipt) string {
	t.Helper()
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
