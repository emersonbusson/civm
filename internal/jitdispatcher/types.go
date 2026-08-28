// Package jitdispatcher dispatches one repository-scoped GitHub Actions JIT
// runner for one allowlisted workflow run.
package jitdispatcher

import (
	"errors"
	"time"
)

const SupportedAPIVersion = "2026-03-10"

const (
	// MachineLockPath is deliberately not configurable. It serializes recovery
	// and admission across every dispatcher configuration on the machine.
	MachineLockPath = "/run/civm/jit-dispatch.lock"
	// LeaseMarkerPath is written by the process that is already running under
	// `guard exec`. EOF never removes it; only an explicit release does.
	LeaseMarkerPath = "/run/civm/jit-dispatch-lease.json"
)

var (
	ErrAmbiguous           = errors.New("remote effect is ambiguous")
	ErrBusy                = errors.New("dispatcher queue wait expired")
	ErrInvalid             = errors.New("invalid dispatcher input")
	ErrReplay              = errors.New("request cannot be replayed")
	ErrStale               = errors.New("candidate ref is stale")
	ErrUnsupportedPlatform = errors.New("JIT dispatcher requires Linux")
)

type Status string

const (
	StatusPrepared           Status = "prepared"
	StatusDispatching        Status = "dispatching"
	StatusWorkflowDispatched Status = "workflow_dispatched"
	StatusRunBound           Status = "run_bound"
	StatusJITCreated         Status = "jit_created"
	StatusRunnerStarted      Status = "runner_started"
	StatusIsolationReady     Status = "isolation_ready"
	StatusReconciling        Status = "reconciling"
	StatusCompleted          Status = "completed"
	StatusFailed             Status = "failed"
	StatusStale              Status = "stale"
	StatusAmbiguous          Status = "ambiguous"
)

type Config struct {
	APIBaseURL      string
	APIVersion      string
	StateDir        string
	RunnerDirectory string
	GuardExecutable string
	IsolationDriver string
	DriverSHA256    string
	BaseImageSHA256 string
	QueueWait       time.Duration
	QueuePoll       time.Duration
	HTTPTimeout     time.Duration
	JobPollInterval time.Duration
	JobBindTimeout  time.Duration
	RunTimeout      time.Duration
	ShutdownGrace   time.Duration
	RecoveryTimeout time.Duration
	Repositories    []RepositoryPolicy
}

type RepositoryPolicy struct {
	Repository     string
	TrustedRef     string
	Workflow       string
	WorkflowSHA256 string
	CandidateRefs  []string
	RunnerGroupID  int64
	JobName        string
}

func (c Config) Policy(repository string) (RepositoryPolicy, bool) {
	for _, policy := range c.Repositories {
		if policy.Repository == repository {
			return policy, true
		}
	}
	return RepositoryPolicy{}, false
}

type Request struct {
	Repository   string
	CandidateRef string
	CandidateSHA string
	Idempotency  string
}

type Identity struct {
	Nonce      string
	Label      string
	RunnerName string
	WorkFolder string
}

type Ledger struct {
	Version         int       `json:"version"`
	RequestID       string    `json:"request_id"`
	Repository      string    `json:"repository"`
	CandidateRef    string    `json:"candidate_ref"`
	CandidateSHA    string    `json:"candidate_sha"`
	TrustedSHA      string    `json:"trusted_sha,omitempty"`
	Workflow        string    `json:"workflow,omitempty"`
	WorkflowSHA256  string    `json:"workflow_sha256,omitempty"`
	Nonce           string    `json:"nonce,omitempty"`
	RunnerLabel     string    `json:"runner_label,omitempty"`
	RunnerName      string    `json:"runner_name,omitempty"`
	WorkFolder      string    `json:"work_folder,omitempty"`
	RunID           int64     `json:"run_id,omitempty"`
	JobID           int64     `json:"job_id,omitempty"`
	RunnerID        int64     `json:"runner_id,omitempty"`
	LeaseID         string    `json:"lease_id,omitempty"`
	ProcessPID      int       `json:"process_pid,omitempty"`
	ProcessStart    uint64    `json:"process_start_ticks,omitempty"`
	ProcessGroup    int       `json:"process_group,omitempty"`
	CgroupPath      string    `json:"cgroup_path,omitempty"`
	IsolationID     string    `json:"isolation_id,omitempty"`
	IsolationBase   string    `json:"isolation_base_sha256,omitempty"`
	RunTerminal     bool      `json:"run_terminal,omitempty"`
	RunnerGone      bool      `json:"runner_gone,omitempty"`
	IsolationGone   bool      `json:"isolation_gone,omitempty"`
	CleanupComplete bool      `json:"cleanup_complete,omitempty"`
	Status          Status    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	FailureCode     string    `json:"failure_code,omitempty"`
}

type Result struct {
	RequestID string `json:"request_id"`
	RunID     int64  `json:"run_id,omitempty"`
	Status    Status `json:"status"`
	Replay    bool   `json:"replay"`
}

type DispatchReceipt struct {
	RunID   int64
	RunURL  string
	HTMLURL string
}

type WorkflowRun struct {
	ID           int64
	Event        string
	HeadSHA      string
	DisplayTitle string
	Path         string
	Status       string
	Conclusion   string
}

type WorkflowJob struct {
	ID            int64
	RunID         int64
	Name          string
	Status        string
	Conclusion    string
	Labels        []string
	RunnerID      int64
	RunnerName    string
	RunnerGroupID int64
}

type RemoteRunner struct {
	ID     int64
	Name   string
	Status string
	Busy   bool
	Labels []RunnerLabel
}

type RunnerLabel struct {
	Name string
	Type string
}

type JITRegistration struct {
	Secret *JITSecret
	Runner RemoteRunner
}

type ProcessIdentity struct {
	PID          int    `json:"pid"`
	StartTicks   uint64 `json:"start_ticks"`
	ProcessGroup int    `json:"process_group"`
	CgroupPath   string `json:"cgroup_path"`
}

type IsolationReceipt struct {
	Protocol       int    `json:"protocol"`
	Event          string `json:"event"`
	LeaseID        string `json:"lease_id"`
	IsolationID    string `json:"isolation_id"`
	BaseSHA256     string `json:"base_sha256"`
	Disposable     bool   `json:"disposable"`
	HostMounts     bool   `json:"host_mounts"`
	HostDocker     bool   `json:"host_docker"`
	ProductSecrets bool   `json:"product_secrets"`
	Destroyed      bool   `json:"destroyed"`
	ResetVerified  bool   `json:"reset_verified"`
}
