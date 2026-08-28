//go:build linux

package jitdispatcher

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type ExecResourceGate struct {
	SelfExecutable string
	MarkerPath     string
	// allowTestMarker is inaccessible outside this package. Its zero value
	// preserves the fixed machine-wide production marker invariant.
	allowTestMarker bool
}

type execResourceLease struct {
	command     *exec.Cmd
	input       io.WriteCloser
	wait        <-chan error
	marker      LeaseMarker
	leaseID     string
	admissionID string
	path        string
}

func NewExecResourceGate(selfExecutable string) *ExecResourceGate {
	return &ExecResourceGate{SelfExecutable: selfExecutable, MarkerPath: LeaseMarkerPath}
}

func (gate *ExecResourceGate) Preflight(guardExecutable string) error {
	if gate == nil || gate.SelfExecutable == "" ||
		(gate.MarkerPath != LeaseMarkerPath && !gate.allowTestMarker) {
		return fmt.Errorf("%w: Guard gate configuration is invalid", ErrInvalid)
	}
	if err := validateTrustedExecutable(guardExecutable, ""); err != nil {
		return fmt.Errorf("Guard executable refused: %w", err)
	}
	if err := validateTrustedExecutable(gate.SelfExecutable, ""); err != nil {
		return fmt.Errorf("civmctl executable refused: %w", err)
	}
	return ensureGateRuntimeDirectory(filepath.Dir(gate.MarkerPath))
}

func (gate *ExecResourceGate) Acquire(
	ctx context.Context,
	guardExecutable, leaseID string,
	poll, grace time.Duration,
) (ResourceLease, error) {
	if gate == nil || gate.SelfExecutable == "" || gate.MarkerPath == "" ||
		!digestRE.MatchString(leaseID) || poll <= 0 || grace <= 0 || grace > time.Minute {
		return nil, fmt.Errorf("%w: Guard gate configuration is invalid", ErrInvalid)
	}
	if err := gate.Preflight(guardExecutable); err != nil {
		return nil, err
	}
	if _, found, err := loadLeaseMarker(gate.MarkerPath); err != nil || found {
		if err == nil {
			err = fmt.Errorf("an unreconciled Guard lease already exists")
		}
		return nil, err
	}
	admission, err := NewIdentity(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("create Guard admission identity: %w", err)
	}
	command := exec.Command(
		guardExecutable, "exec", "--", gate.SelfExecutable,
		"__jit-lease-hold", "--lease-id", leaseID,
		"--admission-id", admission.Nonce,
	)
	command.Env = sanitizedEnvironment()
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	configurePersistentProcess(command)
	input, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("Guard lease stdin: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = input.Close()
		return nil, fmt.Errorf("start Guard resource gate: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	lease := &execResourceLease{
		command: command, input: input, wait: wait,
		leaseID: leaseID, admissionID: admission.Nonce, path: gate.MarkerPath,
	}
	for {
		marker, found, markerErr := loadLeaseMarker(gate.MarkerPath)
		if markerErr != nil {
			cancelErr := lease.cancelPending(grace)
			return nil, errors.Join(fmt.Errorf("%w: Guard admission marker is unreadable", ErrAmbiguous), markerErr, cancelErr)
		}
		if found {
			if marker.LeaseID != leaseID || marker.AdmissionID != admission.Nonce {
				cancelErr := lease.cancelPending(grace)
				return nil, errors.Join(fmt.Errorf("%w: Guard lease marker belongs to another request", ErrAmbiguous), cancelErr)
			}
			alive, aliveErr := processIdentityAlive(marker.HolderPID, marker.StartTicks)
			if aliveErr != nil || !alive {
				cancelErr := lease.cancelPending(grace)
				return nil, errors.Join(fmt.Errorf("%w: Guard lease holder is not alive", ErrAmbiguous), aliveErr, cancelErr)
			}
			lease.marker = marker
			return lease, nil
		}
		timer := time.NewTimer(poll)
		select {
		case waitErr := <-wait:
			if !timer.Stop() {
				<-timer.C
			}
			lease.command = nil
			lease.wait = nil
			_ = input.Close()
			lease.input = nil
			markerErr := cleanupOwnMarker(lease.path, leaseID, admission.Nonce, grace)
			if markerErr != nil {
				return nil, errors.Join(fmt.Errorf("%w: Guard resource gate exited before admission", ErrAmbiguous), waitErr, markerErr)
			}
			return nil, errors.Join(fmt.Errorf("Guard resource gate exited before admission"), waitErr)
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			cancelErr := lease.cancelPending(grace)
			if cancelErr != nil {
				return nil, errors.Join(fmt.Errorf("%w: Guard admission cancellation is unresolved", ErrAmbiguous), cancelErr)
			}
			return nil, fmt.Errorf("%w: Guard admission canceled", ErrBusy)
		case <-timer.C:
		}
	}
}

func (lease *execResourceLease) cancelPending(grace time.Duration) error {
	if lease == nil {
		return nil
	}
	var result error
	if lease.input != nil {
		result = errors.Join(result, lease.input.Close())
		lease.input = nil
	}
	if lease.command != nil && lease.command.Process != nil && lease.wait != nil {
		termErr := signalProcessGroup(lease.command.Process.Pid, false)
		waitErr := waitForExitOrKill(lease.command, lease.wait, grace)
		result = errors.Join(result, termErr, waitErr)
	}
	lease.command = nil
	lease.wait = nil
	result = errors.Join(result, cleanupOwnMarker(lease.path, lease.leaseID, lease.admissionID, grace))
	return result
}

func cleanupOwnMarker(path, leaseID, admissionID string, grace time.Duration) error {
	marker, found, err := loadLeaseMarker(path)
	if err != nil || !found {
		return err
	}
	if marker.LeaseID != leaseID || marker.AdmissionID != admissionID {
		return fmt.Errorf("refusing to remove a Guard marker owned by another lease")
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace*2)
	defer cancel()
	if err := terminateExactProcess(ctx.Done(), marker.HolderPID, marker.StartTicks, grace); err != nil {
		return err
	}
	return removeLeaseMarker(path, marker)
}

func (gate *ExecResourceGate) Orphan() (LeaseMarker, bool, error) {
	if gate == nil || gate.MarkerPath == "" {
		return LeaseMarker{}, false, fmt.Errorf("%w: Guard gate is invalid", ErrInvalid)
	}
	return loadLeaseMarker(gate.MarkerPath)
}

func (gate *ExecResourceGate) ReleaseOrphan(ctx context.Context, marker LeaseMarker, grace time.Duration) error {
	if gate == nil || gate.MarkerPath == "" || grace <= 0 {
		return fmt.Errorf("%w: orphan release parameters are invalid", ErrInvalid)
	}
	if err := validateLeaseMarker(marker); err != nil {
		return err
	}
	if err := terminateExactProcess(ctx.Done(), marker.HolderPID, marker.StartTicks, grace); err != nil {
		return err
	}
	return removeLeaseMarker(gate.MarkerPath, marker)
}

func (lease *execResourceLease) Marker() LeaseMarker { return lease.marker }

func (lease *execResourceLease) Release(ctx context.Context) error {
	if lease == nil || lease.command == nil || lease.input == nil {
		return fmt.Errorf("Guard resource lease is not active")
	}
	_, writeErr := io.WriteString(lease.input, "release\n")
	closeErr := lease.input.Close()
	lease.input = nil
	var waitErr error
	select {
	case waitErr = <-lease.wait:
	case <-ctx.Done():
		waitErr = fmt.Errorf("Guard lease release timed out: %w", ctx.Err())
	}
	marker, found, markerErr := loadLeaseMarker(lease.path)
	if markerErr == nil && found && marker == lease.marker {
		markerErr = fmt.Errorf("Guard lease marker remained after release")
	}
	lease.command = nil
	lease.wait = nil
	return errors.Join(writeErr, closeErr, waitErr, markerErr)
}

func (lease *execResourceLease) Abandon() error {
	if lease == nil {
		return nil
	}
	var result error
	if lease.input != nil {
		result = lease.input.Close()
		lease.input = nil
	}
	lease.command = nil
	lease.wait = nil
	return result
}

func removeStaleLeaseMarker(path string, marker LeaseMarker) error {
	alive, err := processIdentityAlive(marker.HolderPID, marker.StartTicks)
	if err != nil {
		return err
	}
	if alive {
		return fmt.Errorf("refusing to remove a live Guard lease marker")
	}
	return removeLeaseMarker(path, marker)
}

func ensureGateRuntimeDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("Guard gate runtime directory is unsafe")
	}
	return nil
}
