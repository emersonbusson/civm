package jitdispatcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxLogLine = 1 << 20

type RunnerRequest struct {
	DriverExecutable string
	DriverSHA256     string
	BaseImageSHA256  string
	RunnerDirectory  string
	Identity         Identity
	LeaseID          string
	JITConfig        []byte
	LogDirectory     string
	ShutdownGrace    time.Duration
	Sensitive        [][]byte
	OnStarted        func(ProcessIdentity) error
	OnReady          func(IsolationReceipt) error
}

type RecoveryRequest struct {
	DriverExecutable string
	DriverSHA256     string
	BaseImageSHA256  string
	RunnerDirectory  string
	LeaseID          string
	IsolationID      string
	Process          ProcessIdentity
	LogDirectory     string
	ShutdownGrace    time.Duration
	Sensitive        [][]byte
}

type RunnerOutcome struct {
	Process   ProcessIdentity
	Ready     IsolationReceipt
	Destroyed IsolationReceipt
}

type Runner interface {
	Preflight(Config) error
	Run(context.Context, RunnerRequest) (RunnerOutcome, error)
	Recover(context.Context, RecoveryRequest) (IsolationReceipt, error)
}

type ExecRunner struct {
	Containment ProcessContainment
}

func NewExecRunner() ExecRunner { return ExecRunner{Containment: CgroupContainment{}} }

func (runner ExecRunner) Preflight(config Config) error {
	if runner.Containment == nil {
		return fmt.Errorf("%w: process containment is unavailable", ErrInvalid)
	}
	if err := validateTrustedExecutable(config.IsolationDriver, config.DriverSHA256); err != nil {
		return fmt.Errorf("isolation driver refused: %w", err)
	}
	return validateTrustedDirectory("runner_directory", config.RunnerDirectory)
}

type driverInput struct {
	Protocol  int    `json:"protocol"`
	LeaseID   string `json:"lease_id"`
	JITConfig string `json:"encoded_jit_config"`
}

func (runner ExecRunner) Run(ctx context.Context, request RunnerRequest) (RunnerOutcome, error) {
	if err := validateRunnerRequest(request, runner.Containment); err != nil {
		return RunnerOutcome{}, err
	}
	arguments := []string{
		"run", "--protocol=1", "--lease-id=" + request.LeaseID,
		"--runner-directory=" + request.RunnerDirectory,
		"--expected-base-sha256=" + request.BaseImageSHA256,
		"--control-fd=3",
	}
	return runner.runDriver(ctx, request, arguments)
}

func (runner ExecRunner) Recover(ctx context.Context, request RecoveryRequest) (IsolationReceipt, error) {
	if err := validateRecoveryRequest(request, runner.Containment); err != nil {
		return IsolationReceipt{}, err
	}
	if err := runner.Containment.Recover(ctx, request.LeaseID, request.Process, request.ShutdownGrace); err != nil {
		return IsolationReceipt{}, fmt.Errorf("terminate orphaned driver containment: %w", err)
	}
	if err := validateTrustedExecutable(request.DriverExecutable, request.DriverSHA256); err != nil {
		return IsolationReceipt{}, err
	}
	arguments := []string{
		"recover", "--protocol=1", "--lease-id=" + request.LeaseID,
		"--runner-directory=" + request.RunnerDirectory,
		"--expected-base-sha256=" + request.BaseImageSHA256,
		"--control-fd=3",
	}
	if request.IsolationID != "" {
		arguments = append(arguments, "--isolation-id="+request.IsolationID)
	}
	receipts, _, err := runner.execDriver(ctx, driverExecRequest{
		Executable: request.DriverExecutable, ExecutableSHA256: request.DriverSHA256,
		Arguments: arguments, LeaseID: request.LeaseID,
		BaseImageSHA256: request.BaseImageSHA256,
		LogPath:         filepath.Join(request.LogDirectory, "recovery.log"),
		ShutdownGrace:   request.ShutdownGrace, Sensitive: request.Sensitive,
		Containment: runner.Containment,
	})
	if err != nil {
		return IsolationReceipt{}, err
	}
	if len(receipts) != 1 || !validDestroyedReceipt(receipts[0], request.LeaseID, request.IsolationID, request.BaseImageSHA256) {
		return IsolationReceipt{}, fmt.Errorf("recovery driver did not prove disposable isolation destruction")
	}
	return receipts[0], nil
}

func (runner ExecRunner) runDriver(
	ctx context.Context,
	request RunnerRequest,
	arguments []string,
) (RunnerOutcome, error) {
	input, err := json.Marshal(driverInput{Protocol: 1, LeaseID: request.LeaseID, JITConfig: string(request.JITConfig)})
	if err != nil {
		return RunnerOutcome{}, err
	}
	input = append(input, '\n')
	defer Zero(input)
	receipts, process, err := runner.execDriver(ctx, driverExecRequest{
		Executable: request.DriverExecutable, ExecutableSHA256: request.DriverSHA256,
		Arguments: arguments, LeaseID: request.LeaseID,
		BaseImageSHA256: request.BaseImageSHA256,
		LogPath:         filepath.Join(request.LogDirectory, "isolation-driver.log"),
		ShutdownGrace:   request.ShutdownGrace, Sensitive: append(request.Sensitive, request.JITConfig),
		Containment: runner.Containment, Input: input,
		OnStarted: request.OnStarted, OnReady: request.OnReady,
	})
	outcome := RunnerOutcome{Process: process}
	if len(receipts) > 0 {
		outcome.Ready = receipts[0]
	}
	if len(receipts) > 1 {
		outcome.Destroyed = receipts[1]
	}
	if err != nil {
		return outcome, err
	}
	if len(receipts) != 2 || !validReadyReceipt(receipts[0], request.LeaseID, request.BaseImageSHA256) ||
		!validDestroyedReceipt(receipts[1], request.LeaseID, receipts[0].IsolationID, request.BaseImageSHA256) {
		return outcome, fmt.Errorf("isolation driver lifecycle proof is incomplete")
	}
	return outcome, nil
}

type driverExecRequest struct {
	Executable       string
	ExecutableSHA256 string
	Arguments        []string
	LeaseID          string
	BaseImageSHA256  string
	LogPath          string
	ShutdownGrace    time.Duration
	Sensitive        [][]byte
	Containment      ProcessContainment
	Input            []byte
	OnStarted        func(ProcessIdentity) error
	OnReady          func(IsolationReceipt) error
}

func (runner ExecRunner) execDriver(
	ctx context.Context,
	request driverExecRequest,
) ([]IsolationReceipt, ProcessIdentity, error) {
	executable, err := openTrustedExecutable(request.Executable, request.ExecutableSHA256)
	if err != nil {
		return nil, ProcessIdentity{}, err
	}
	defer func() { _ = executable.Close() }()
	logFile, err := openLog(request.LogPath, request.Input == nil)
	if err != nil {
		return nil, ProcessIdentity{}, err
	}
	defer func() { _ = logFile.Close() }()
	redactions := normalizedRedactions(request.Sensitive)
	defer zeroRedactions(redactions)
	lockedLog := &lockedWriter{writer: logFile}
	stdout := newLineRedactor(lockedLog, []byte("stdout: "), redactions)
	stderr := newLineRedactor(lockedLog, []byte("stderr: "), redactions)
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, ProcessIdentity{}, err
	}
	defer func() { _ = controlRead.Close() }()
	command := exec.Command("/proc/self/fd/4", request.Arguments...)
	command.Args[0] = request.Executable
	command.Env = sanitizedEnvironment()
	command.Stdout = stdout
	command.Stderr = stderr
	command.ExtraFiles = []*os.File{controlWrite, executable}
	command.WaitDelay = request.ShutdownGrace
	configureProcess(command)
	preparation, err := request.Containment.Prepare(command, request.LeaseID)
	if err != nil {
		_ = controlWrite.Close()
		return nil, ProcessIdentity{}, fmt.Errorf("prepare isolation containment: %w", err)
	}
	input, err := command.StdinPipe()
	if err != nil {
		closeErr := preparation.Close()
		recoveryErr := request.Containment.Recover(context.Background(), request.LeaseID, ProcessIdentity{}, request.ShutdownGrace)
		_ = controlWrite.Close()
		return nil, ProcessIdentity{}, errors.Join(err, closeErr, recoveryErr)
	}
	if err := command.Start(); err != nil {
		closeErr := preparation.Close()
		recoveryErr := request.Containment.Recover(context.Background(), request.LeaseID, ProcessIdentity{}, request.ShutdownGrace)
		_ = controlWrite.Close()
		_ = input.Close()
		return nil, ProcessIdentity{}, errors.Join(fmt.Errorf("start isolation driver: %w", err), closeErr, recoveryErr)
	}
	_ = controlWrite.Close()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	if err := preparation.Close(); err != nil {
		_ = input.Close()
		return nil, ProcessIdentity{}, errors.Join(err, stopUnidentifiedAndWait(wait, request.Containment, request.LeaseID, request.ShutdownGrace))
	}
	process, err := request.Containment.Attach(command.Process.Pid, request.LeaseID)
	if err != nil {
		_ = input.Close()
		return nil, ProcessIdentity{}, errors.Join(err, stopUnidentifiedAndWait(wait, request.Containment, request.LeaseID, request.ShutdownGrace))
	}
	if request.OnStarted != nil {
		if err := request.OnStarted(process); err != nil {
			_ = input.Close()
			return nil, process, errors.Join(err, stopAndWait(command, wait, request.Containment, process, request.ShutdownGrace))
		}
	}
	reader := bufio.NewReader(io.LimitReader(controlRead, (64<<10)+1))
	first, err := readReceipt(ctx, reader)
	if err != nil {
		_ = input.Close()
		return nil, process, errors.Join(err, stopAndWait(command, wait, request.Containment, process, request.ShutdownGrace))
	}
	receipts := []IsolationReceipt{first}
	if request.Input != nil {
		if !validReadyReceipt(first, request.LeaseID, request.BaseImageSHA256) {
			_ = input.Close()
			return receipts, process, errors.Join(fmt.Errorf("isolation ready proof is invalid"), stopAndWait(command, wait, request.Containment, process, request.ShutdownGrace))
		}
		if request.OnReady != nil {
			if err := request.OnReady(first); err != nil {
				_ = input.Close()
				return receipts, process, errors.Join(err, stopAndWait(command, wait, request.Containment, process, request.ShutdownGrace))
			}
		}
		if _, err := input.Write(request.Input); err != nil {
			_ = input.Close()
			return receipts, process, errors.Join(err, stopAndWait(command, wait, request.Containment, process, request.ShutdownGrace))
		}
	}
	if err := input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return receipts, process, errors.Join(err, stopAndWait(command, wait, request.Containment, process, request.ShutdownGrace))
	}
	var receiptErr error
	if request.Input != nil {
		var second IsolationReceipt
		second, receiptErr = readReceipt(ctx, reader)
		if receiptErr == nil {
			receipts = append(receipts, second)
		}
	}
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		waitErr = errors.Join(ctx.Err(), stopAndWait(command, wait, request.Containment, process, request.ShutdownGrace))
	}
	containmentErr := request.Containment.Terminate(context.Background(), process, request.ShutdownGrace)
	flushErr := errors.Join(stdout.Flush(), stderr.Flush())
	if waitErr != nil {
		waitErr = fmt.Errorf("isolation driver exited: %w", waitErr)
	}
	return receipts, process, errors.Join(receiptErr, waitErr, containmentErr, flushErr)
}

func validateRunnerRequest(request RunnerRequest, containment ProcessContainment) error {
	if containment == nil || !validIdentity(request.Identity) || !digestRE.MatchString(request.LeaseID) ||
		!digestRE.MatchString(request.DriverSHA256) || !digestRE.MatchString(request.BaseImageSHA256) ||
		len(request.JITConfig) < 16 || len(request.JITConfig) > maxJITResponse {
		return fmt.Errorf("%w: isolated runner identity is invalid", ErrInvalid)
	}
	if err := validateAbsoluteDir("runner_directory", request.RunnerDirectory); err != nil {
		return err
	}
	if err := validateAbsoluteDir("log_directory", request.LogDirectory); err != nil {
		return err
	}
	if request.ShutdownGrace <= 0 || request.ShutdownGrace > time.Minute {
		return fmt.Errorf("%w: shutdown grace is invalid", ErrInvalid)
	}
	if err := validateTrustedExecutable(request.DriverExecutable, request.DriverSHA256); err != nil {
		return err
	}
	for _, char := range request.JITConfig {
		if char <= 0x20 || char >= 0x7f {
			return fmt.Errorf("%w: JIT config contains unsafe bytes", ErrInvalid)
		}
	}
	info, err := os.Lstat(request.LogDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("log directory is not an owner-only real directory")
	}
	return nil
}

func validateRecoveryRequest(request RecoveryRequest, containment ProcessContainment) error {
	if containment == nil || !digestRE.MatchString(request.LeaseID) || !digestRE.MatchString(request.DriverSHA256) ||
		!digestRE.MatchString(request.BaseImageSHA256) || request.ShutdownGrace <= 0 || request.ShutdownGrace > time.Minute {
		return fmt.Errorf("%w: isolation recovery identity is invalid", ErrInvalid)
	}
	if request.IsolationID != "" && !safeOpaqueID(request.IsolationID) {
		return fmt.Errorf("%w: isolation ID is invalid", ErrInvalid)
	}
	if err := validateAbsoluteDir("runner_directory", request.RunnerDirectory); err != nil {
		return err
	}
	return validateAbsoluteDir("log_directory", request.LogDirectory)
}

func readReceipt(ctx context.Context, reader *bufio.Reader) (IsolationReceipt, error) {
	type response struct {
		receipt IsolationReceipt
		err     error
	}
	result := make(chan response, 1)
	go func() {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			result <- response{err: err}
			return
		}
		if len(line) == 0 || len(line) > 64<<10 {
			result <- response{err: fmt.Errorf("isolation control message exceeds safety limit")}
			return
		}
		var receipt IsolationReceipt
		if err := decodeStrictJSON(line, &receipt); err != nil {
			result <- response{err: fmt.Errorf("isolation control JSON: %w", err)}
			return
		}
		result <- response{receipt: receipt}
	}()
	select {
	case <-ctx.Done():
		return IsolationReceipt{}, ctx.Err()
	case value := <-result:
		return value.receipt, value.err
	}
}

func validReadyReceipt(receipt IsolationReceipt, leaseID, baseSHA256 string) bool {
	return receipt.Protocol == 1 && receipt.Event == "ready" && receipt.LeaseID == leaseID &&
		safeOpaqueID(receipt.IsolationID) && receipt.BaseSHA256 == baseSHA256 && receipt.Disposable &&
		!receipt.HostMounts && !receipt.HostDocker && !receipt.ProductSecrets &&
		!receipt.Destroyed && !receipt.ResetVerified
}

func validDestroyedReceipt(receipt IsolationReceipt, leaseID, isolationID, baseSHA256 string) bool {
	return receipt.Protocol == 1 && receipt.Event == "destroyed" && receipt.LeaseID == leaseID &&
		receipt.IsolationID != "" && (isolationID == "" || receipt.IsolationID == isolationID) &&
		receipt.BaseSHA256 == baseSHA256 && receipt.Disposable && !receipt.HostMounts &&
		!receipt.HostDocker && !receipt.ProductSecrets && receipt.Destroyed && receipt.ResetVerified
}

func safeOpaqueID(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:-", char) {
			continue
		}
		return false
	}
	return true
}

func stopAndWait(
	command *exec.Cmd,
	wait <-chan error,
	containment ProcessContainment,
	identity ProcessIdentity,
	grace time.Duration,
) error {
	if command == nil || command.Process == nil {
		return nil
	}
	if identity.PID > 0 {
		terminateErr := containment.Terminate(context.Background(), identity, grace)
		return errors.Join(terminateErr, waitForExit(wait, grace))
	}
	termErr := signalProcessGroup(command.Process.Pid, false)
	if wait == nil {
		return termErr
	}
	return errors.Join(termErr, waitForExitOrKill(command, wait, grace))
}

func waitForExit(wait <-chan error, grace time.Duration) error {
	if wait == nil {
		return nil
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case waitErr := <-wait:
		return ignoreSignalExit(waitErr)
	case <-timer.C:
		return fmt.Errorf("isolation driver was not reaped before timeout")
	}
}

func waitForExitOrKill(command *exec.Cmd, wait <-chan error, grace time.Duration) error {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case waitErr := <-wait:
		return ignoreSignalExit(waitErr)
	case <-timer.C:
		killErr := signalProcessGroup(command.Process.Pid, true)
		return errors.Join(killErr, waitForExit(wait, grace))
	}
}

func stopUnidentifiedAndWait(
	wait <-chan error,
	containment ProcessContainment,
	leaseID string,
	grace time.Duration,
) error {
	recoveryErr := containment.Recover(context.Background(), leaseID, ProcessIdentity{}, grace)
	return errors.Join(recoveryErr, waitForExit(wait, grace))
}

func ignoreSignalExit(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}

func sanitizedEnvironment() []string {
	allowed := []string{"HOME", "LANG", "LC_ALL", "PATH", "TMPDIR"}
	environment := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok && !strings.ContainsAny(value, "\x00\r\n") {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *lockedWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(data)
}

type lineRedactor struct {
	mu          sync.Mutex
	destination io.Writer
	prefix      []byte
	redactions  [][]byte
	pending     []byte
}

func newLineRedactor(destination io.Writer, prefix []byte, redactions [][]byte) *lineRedactor {
	return &lineRedactor{destination: destination, prefix: prefix, redactions: redactions}
}

func (writer *lineRedactor) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	written := len(data)
	writer.pending = append(writer.pending, data...)
	if len(writer.pending) > maxLogLine && !bytes.ContainsRune(writer.pending, '\n') {
		return 0, fmt.Errorf("runner log line exceeds safety limit")
	}
	for {
		index := bytes.IndexByte(writer.pending, '\n')
		if index < 0 {
			break
		}
		line := append([]byte(nil), writer.pending[:index]...)
		Zero(writer.pending[:index+1])
		writer.pending = writer.pending[index+1:]
		if err := writer.writeLine(line); err != nil {
			return 0, err
		}
	}
	return written, nil
}

func (writer *lineRedactor) Flush() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.pending) == 0 {
		return nil
	}
	line := append([]byte(nil), writer.pending...)
	Zero(writer.pending)
	writer.pending = nil
	return writer.writeLine(line)
}

func (writer *lineRedactor) writeLine(line []byte) error {
	for _, secret := range writer.redactions {
		if bytes.Contains(line, secret) {
			redacted := bytes.ReplaceAll(line, secret, []byte("[REDACTED]"))
			Zero(line)
			line = redacted
		}
	}
	defer Zero(line)
	if _, err := writer.destination.Write(writer.prefix); err != nil {
		return err
	}
	if _, err := writer.destination.Write(line); err != nil {
		return err
	}
	_, err := writer.destination.Write([]byte{'\n'})
	return err
}

func normalizedRedactions(values [][]byte) [][]byte {
	result := make([][]byte, 0, len(values))
	for _, value := range values {
		if len(value) < 4 {
			continue
		}
		duplicate := false
		for _, existing := range result {
			if bytes.Equal(existing, value) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, append([]byte(nil), value...))
		}
	}
	sort.Slice(result, func(left, right int) bool { return len(result[left]) > len(result[right]) })
	return result
}

func zeroRedactions(values [][]byte) {
	for _, value := range values {
		Zero(value)
	}
}

func openLog(path string, appendExisting bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if appendExisting {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	file, err := openFileNoFollow(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open isolation log: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		_ = file.Close()
		return nil, errors.Join(fmt.Errorf("isolation log metadata is unsafe"), err)
	}
	return file, nil
}
