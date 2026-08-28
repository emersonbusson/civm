package jitdispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type ResourceLease interface {
	Marker() LeaseMarker
	Release(context.Context) error
	Abandon() error
}

type ResourceGate interface {
	Preflight(string) error
	Acquire(context.Context, string, string, time.Duration, time.Duration) (ResourceLease, error)
	Orphan() (LeaseMarker, bool, error)
	ReleaseOrphan(context.Context, LeaseMarker, time.Duration) error
}

type LeaseMarker struct {
	Version     int    `json:"version"`
	LeaseID     string `json:"lease_id"`
	AdmissionID string `json:"admission_id"`
	HolderPID   int    `json:"holder_pid"`
	StartTicks  uint64 `json:"start_ticks"`
}

func validateLeaseMarker(marker LeaseMarker) error {
	if marker.Version != 2 || !digestRE.MatchString(marker.LeaseID) ||
		!digestRE.MatchString(marker.AdmissionID) || marker.HolderPID <= 1 || marker.StartTicks == 0 {
		return fmt.Errorf("%w: Guard lease marker is invalid", ErrInvalid)
	}
	return nil
}

func loadLeaseMarker(path string) (LeaseMarker, bool, error) {
	data, found, err := readOwnerOnlyRegular(path, 4096)
	if err != nil || !found {
		return LeaseMarker{}, found, err
	}
	var marker LeaseMarker
	if err := decodeStrictJSON(data, &marker); err != nil {
		return LeaseMarker{}, false, fmt.Errorf("lease marker JSON: %w", err)
	}
	if err := validateLeaseMarker(marker); err != nil {
		return LeaseMarker{}, false, err
	}
	return marker, true, nil
}

func writeLeaseMarker(path string, marker LeaseMarker) error {
	if err := validateLeaseMarker(marker); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := rejectSymlinkComponents(directory); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("lease marker directory is not trusted")
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("lease marker create: %w", err)
	}
	wrote := false
	defer func() {
		_ = file.Close()
		if !wrote {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("lease marker write: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("lease marker fsync: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("lease marker close: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return err
	}
	wrote = true
	return nil
}

func removeLeaseMarker(path string, expected LeaseMarker) error {
	current, found, err := loadLeaseMarker(path)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if current != expected {
		return fmt.Errorf("lease marker identity changed")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("lease marker remove: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

// RunLeaseHolder is the private child invoked only through `guard exec`. EOF
// means the dispatcher died: the holder intentionally remains alive so Guard
// keeps the machine-wide lease until startup reconciliation proves cleanup.
func RunLeaseHolder(input io.Reader, leaseID, admissionID, markerPath string) error {
	if !digestRE.MatchString(leaseID) || !digestRE.MatchString(admissionID) {
		return fmt.Errorf("%w: lease or admission ID is invalid", ErrInvalid)
	}
	start, err := currentProcessStart()
	if err != nil {
		return err
	}
	marker := LeaseMarker{
		Version: 2, LeaseID: leaseID, AdmissionID: admissionID,
		HolderPID: os.Getpid(), StartTicks: start,
	}
	if err := writeLeaseMarker(markerPath, marker); err != nil {
		return err
	}
	buffer := make([]byte, len("release\n"))
	_, readErr := io.ReadFull(input, buffer)
	if readErr == nil && string(buffer) == "release\n" {
		return removeLeaseMarker(markerPath, marker)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return readErr
	}
	select {}
}
