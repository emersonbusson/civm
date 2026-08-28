package jitdispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const ledgerVersion = 2

var requestIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Store struct {
	dir       string
	ledgerDir string
	logDir    string
}

func OpenStore(dir string) (*Store, error) {
	if err := validateAbsoluteDir("state_dir", dir); err != nil {
		return nil, err
	}
	if err := ensureDirectory(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	store := &Store{
		dir: dir, ledgerDir: filepath.Join(dir, "requests"), logDir: filepath.Join(dir, "logs"),
	}
	for _, path := range []string{store.ledgerDir, store.logDir} {
		if err := ensureDirectory(path, 0o700); err != nil {
			return nil, fmt.Errorf("state child directory: %w", err)
		}
	}
	return store, nil
}

func RequestID(request Request) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		request.Repository, request.CandidateRef, request.CandidateSHA, request.Idempotency,
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func (s *Store) RequestLogDir(requestID string) (string, error) {
	if !requestIDRE.MatchString(requestID) {
		return "", fmt.Errorf("%w: request ID is invalid", ErrInvalid)
	}
	path := filepath.Join(s.logDir, requestID)
	if err := ensureDirectory(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) List() ([]Ledger, error) {
	entries, err := os.ReadDir(s.ledgerDir)
	if err != nil {
		return nil, fmt.Errorf("ledger directory read: %w", err)
	}
	ledgers := make([]Ledger, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(name, ".json") {
			return nil, fmt.Errorf("ledger directory contains an unexpected entry")
		}
		requestID := strings.TrimSuffix(name, ".json")
		ledger, found, loadErr := s.Load(requestID)
		if loadErr != nil || !found {
			return nil, errors.Join(fmt.Errorf("load ledger %s", requestID), loadErr)
		}
		ledgers = append(ledgers, ledger)
	}
	sort.Slice(ledgers, func(left, right int) bool {
		return ledgers[left].CreatedAt.Before(ledgers[right].CreatedAt)
	})
	return ledgers, nil
}

func (s *Store) Load(requestID string) (Ledger, bool, error) {
	path, err := s.ledgerPath(requestID)
	if err != nil {
		return Ledger{}, false, err
	}
	data, found, err := readOwnerOnlyRegular(path, 64<<10)
	if err != nil || !found {
		return Ledger{}, found, err
	}
	var ledger Ledger
	if err := decodeStrictJSON(data, &ledger); err != nil {
		return Ledger{}, false, fmt.Errorf("ledger JSON: %w", err)
	}
	if err := validateLedger(ledger, requestID); err != nil {
		return Ledger{}, false, err
	}
	return ledger, true, nil
}

func (s *Store) Save(ledger Ledger) error {
	if err := validateLedger(ledger, ledger.RequestID); err != nil {
		return err
	}
	path, err := s.ledgerPath(ledger.RequestID)
	if err != nil {
		return err
	}
	data, err := marshalLedger(ledger)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.ledgerDir, ".ledger-*")
	if err != nil {
		return fmt.Errorf("ledger temp create: %w", err)
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("ledger chmod: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("ledger write: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("ledger fsync: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("ledger close: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("ledger rename: %w", err)
	}
	if err := syncDirectory(s.ledgerDir); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *Store) ledgerPath(requestID string) (string, error) {
	if !requestIDRE.MatchString(requestID) {
		return "", fmt.Errorf("%w: request ID is invalid", ErrInvalid)
	}
	return filepath.Join(s.ledgerDir, requestID+".json"), nil
}

func validateLedger(ledger Ledger, expectedID string) error {
	if ledger.Version != ledgerVersion || ledger.RequestID != expectedID || !requestIDRE.MatchString(ledger.RequestID) {
		return fmt.Errorf("ledger identity or version is invalid")
	}
	if validateRepository(ledger.Repository) != nil || validateRef(ledger.CandidateRef) != nil ||
		!shaRE.MatchString(ledger.CandidateSHA) || !shaRE.MatchString(ledger.TrustedSHA) ||
		!workflowRE.MatchString(ledger.Workflow) || !digestRE.MatchString(ledger.WorkflowSHA256) {
		return fmt.Errorf("ledger request fields are invalid")
	}
	identity := Identity{
		Nonce: ledger.Nonce, Label: ledger.RunnerLabel,
		RunnerName: ledger.RunnerName, WorkFolder: ledger.WorkFolder,
	}
	if !validIdentity(identity) || ledger.LeaseID != ledger.Nonce {
		return fmt.Errorf("ledger runner or lease identity is invalid")
	}
	if ledger.CreatedAt.IsZero() || ledger.UpdatedAt.Before(ledger.CreatedAt) || !validStatus(ledger.Status) {
		return fmt.Errorf("ledger timestamps or status are invalid")
	}
	if ledger.RunID < 0 || ledger.JobID < 0 || ledger.RunnerID < 0 || ledger.ProcessPID < 0 || ledger.ProcessGroup < 0 {
		return fmt.Errorf("ledger numeric identity is invalid")
	}
	if (ledger.RunID == 0 && (ledger.JobID != 0 || ledger.RunnerID != 0)) ||
		(ledger.JobID == 0 && ledger.RunnerID != 0) {
		return fmt.Errorf("ledger remote identity ordering is invalid")
	}
	if ledger.ProcessPID == 0 && (ledger.ProcessStart != 0 || ledger.ProcessGroup != 0 || ledger.CgroupPath != "") {
		return fmt.Errorf("ledger process identity is incomplete")
	}
	if ledger.ProcessPID > 0 && (ledger.RunnerID == 0 || ledger.ProcessStart == 0 || ledger.ProcessGroup != ledger.ProcessPID ||
		!filepath.IsAbs(ledger.CgroupPath) || filepath.Clean(ledger.CgroupPath) != ledger.CgroupPath ||
		filepath.Base(ledger.CgroupPath) != "civm-jit-"+ledger.LeaseID) {
		return fmt.Errorf("ledger process identity is incomplete")
	}
	if (ledger.IsolationID == "") != (ledger.IsolationBase == "") ||
		(ledger.IsolationID != "" && (!safeOpaqueID(ledger.IsolationID) ||
			!digestRE.MatchString(ledger.IsolationBase) || ledger.RunnerID == 0)) {
		return fmt.Errorf("ledger isolation base identity is invalid")
	}
	if ledger.IsolationGone && ledger.ProcessPID != 0 {
		return fmt.Errorf("ledger marks isolation gone while its process remains")
	}
	if ledger.CleanupComplete && (!ledger.RunTerminal || !ledger.RunnerGone || !ledger.IsolationGone) {
		return fmt.Errorf("ledger cleanup proof is incomplete")
	}
	return nil
}

func validStatus(status Status) bool {
	switch status {
	case StatusPrepared, StatusDispatching, StatusWorkflowDispatched, StatusRunBound,
		StatusJITCreated, StatusRunnerStarted, StatusIsolationReady, StatusReconciling,
		StatusCompleted, StatusFailed,
		StatusStale, StatusAmbiguous:
		return true
	default:
		return false
	}
}

func marshalLedger(ledger Ledger) ([]byte, error) {
	data, err := jsonMarshalIndent(ledger)
	if err != nil {
		return nil, fmt.Errorf("ledger encode: %w", err)
	}
	return append(data, '\n'), nil
}

func ensureDirectory(path string, mode os.FileMode) error {
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("%s is not an owner-only directory", path)
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	clean := filepath.Clean(path)
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component is forbidden: %s", current)
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("directory open: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("directory fsync: %w", err)
	}
	return nil
}
