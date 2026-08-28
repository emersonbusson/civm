package jitdispatcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreAtomicRoundTripAndRequestID(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	request := Request{"acme/site", "refs/heads/feature/safe", strings.Repeat("a", 40), "secret-idempotency-key"}
	requestID := RequestID(request)
	if strings.Contains(requestID, request.Idempotency) || !requestIDRE.MatchString(requestID) {
		t.Fatalf("unsafe request ID %q", requestID)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	identity, err := NewIdentity(strings.NewReader(strings.Repeat("i", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ledger := Ledger{
		Version: ledgerVersion, RequestID: requestID, Repository: request.Repository,
		CandidateRef: request.CandidateRef, CandidateSHA: request.CandidateSHA,
		TrustedSHA: strings.Repeat("b", 40), Workflow: ".github/workflows/civm-jit.yml",
		WorkflowSHA256: strings.Repeat("c", 64), Nonce: identity.Nonce,
		RunnerLabel: identity.Label, RunnerName: identity.RunnerName,
		WorkFolder: identity.WorkFolder, LeaseID: identity.Nonce,
		Status: StatusPrepared, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Save(ledger); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.Load(requestID)
	if err != nil || !found || loaded.Status != StatusPrepared {
		t.Fatalf("Load() = %+v, %v, %v", loaded, found, err)
	}
	tampered := ledger
	tampered.RunnerName = "civm-jit-wrong00000000000"
	if err := store.Save(tampered); err == nil {
		t.Fatal("Save() accepted a runner identity unrelated to the nonce")
	}
	tampered = ledger
	tampered.RunID = 1
	tampered.JobID = 2
	tampered.RunnerID = 3
	tampered.ProcessPID = 123
	tampered.ProcessStart = 456
	tampered.ProcessGroup = 124
	tampered.CgroupPath = "/sys/fs/cgroup/civm-jit-" + ledger.LeaseID
	if err := store.Save(tampered); err == nil {
		t.Fatal("Save() accepted a process group different from the exact PID")
	}
	info, err := os.Stat(filepath.Join(store.ledgerDir, requestID+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %v, error = %v", info.Mode().Perm(), err)
	}
	if _, found, err := store.Load(strings.Repeat("b", 64)); err != nil || found {
		t.Fatalf("missing Load() found=%v err=%v", found, err)
	}
}

func TestStoreRejectsCorruptionPermissionsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	requestID := strings.Repeat("c", 64)
	path := filepath.Join(store.ledgerDir, requestID+".json")
	if err := os.WriteFile(path, []byte(`{"version":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(requestID); err == nil {
		t.Fatal("Load() accepted truncated ledger")
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // test file permission
		t.Fatal(err)
	}
	if _, _, err := store.Load(requestID); err == nil {
		t.Fatal("Load() accepted permissive ledger")
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(filepath.Join(link, "state")); err == nil {
		t.Fatal("OpenStore() accepted a symlink component")
	}
	if _, _, err := store.Load("bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid request ID error = %v", err)
	}
}

func TestStoreRequestLogDirectoryAndInvalidLedger(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	requestID := strings.Repeat("d", 64)
	logDirectory, err := store.RequestLogDir(requestID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(logDirectory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode/error = %v/%v", info.Mode().Perm(), err)
	}
	if _, err := store.RequestLogDir("bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid log request ID error = %v", err)
	}
	if err := store.Save(Ledger{Version: ledgerVersion, RequestID: requestID}); err == nil {
		t.Fatal("Save() accepted incomplete ledger")
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	partialProcess := Ledger{
		Version: ledgerVersion, RequestID: requestID, Repository: "acme/site",
		CandidateRef: "refs/heads/feature/safe", CandidateSHA: strings.Repeat("a", 40),
		TrustedSHA: strings.Repeat("b", 40), Workflow: ".github/workflows/civm-jit.yml",
		WorkflowSHA256: strings.Repeat("c", 64), Nonce: strings.Repeat("d", 64),
		RunnerLabel: "civm-jit-" + strings.Repeat("d", 64),
		RunnerName:  "civm-jit-" + strings.Repeat("d", 16),
		WorkFolder:  "_work/jit-" + strings.Repeat("d", 64),
		LeaseID:     strings.Repeat("d", 64), RunID: 1, JobID: 2, RunnerID: 3,
		ProcessPID: 123, ProcessGroup: 123,
		CgroupPath: "/sys/fs/cgroup/civm-jit-test", Status: StatusRunnerStarted,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Save(partialProcess); err == nil {
		t.Fatal("Save() accepted PID without exact start identity")
	}
	if validStatus("unknown") {
		t.Fatal("validStatus() accepted unknown state")
	}
}
