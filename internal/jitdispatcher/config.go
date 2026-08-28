package jitdispatcher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxConfigBytes = 256 << 10

var (
	shaRE         = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	digestRE      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	workflowRE    = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.ya?ml$`)
	idempotencyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
	jobNameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:/()-]{0,99}$`)
)

type rawConfig struct {
	APIBaseURL      string          `json:"api_base_url"`
	APIVersion      string          `json:"api_version"`
	StateDir        string          `json:"state_dir"`
	RunnerDirectory string          `json:"runner_directory"`
	GuardExecutable string          `json:"guard_executable"`
	IsolationDriver string          `json:"isolation_driver"`
	DriverSHA256    string          `json:"isolation_driver_sha256"`
	BaseImageSHA256 string          `json:"base_image_sha256"`
	QueueWait       string          `json:"queue_wait"`
	QueuePoll       string          `json:"queue_poll"`
	HTTPTimeout     string          `json:"http_timeout"`
	JobPollInterval string          `json:"job_poll_interval"`
	JobBindTimeout  string          `json:"job_bind_timeout"`
	RunTimeout      string          `json:"run_timeout"`
	ShutdownGrace   string          `json:"shutdown_grace"`
	RecoveryTimeout string          `json:"recovery_timeout"`
	Repositories    []rawRepository `json:"repositories"`
}

type rawRepository struct {
	Repository     string   `json:"repository"`
	TrustedRef     string   `json:"trusted_ref"`
	Workflow       string   `json:"workflow"`
	WorkflowSHA256 string   `json:"workflow_sha256"`
	CandidateRefs  []string `json:"candidate_refs"`
	RunnerGroupID  int64    `json:"runner_group_id"`
	JobName        string   `json:"job_name"`
}

func LoadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Config{}, fmt.Errorf("%w: config path must be absolute and clean", ErrInvalid)
	}
	data, found, err := readOwnerOnlyRegular(path, maxConfigBytes)
	if err != nil {
		return Config{}, fmt.Errorf("%w: config must be an owner-only regular file: %w", ErrInvalid, err)
	}
	if !found {
		return Config{}, fmt.Errorf("%w: config file does not exist", ErrInvalid)
	}
	var raw rawConfig
	if err := decodeStrictJSON(data, &raw); err != nil {
		return Config{}, fmt.Errorf("%w: config JSON: %w", ErrInvalid, err)
	}
	return validateConfig(raw)
}

func validateConfig(raw rawConfig) (Config, error) {
	if raw.APIVersion != SupportedAPIVersion {
		return Config{}, fmt.Errorf("%w: api_version must be %s", ErrInvalid, SupportedAPIVersion)
	}
	if err := validateAPIBase(raw.APIBaseURL); err != nil {
		return Config{}, err
	}
	if err := validateAbsoluteDir("state_dir", raw.StateDir); err != nil {
		return Config{}, err
	}
	if err := validateAbsoluteDir("runner_directory", raw.RunnerDirectory); err != nil {
		return Config{}, err
	}
	if pathsOverlap(raw.StateDir, raw.RunnerDirectory) {
		return Config{}, fmt.Errorf("%w: state_dir and runner_directory must not overlap", ErrInvalid)
	}
	if err := validateAbsoluteExecutable("guard_executable", raw.GuardExecutable); err != nil {
		return Config{}, err
	}
	if err := validateAbsoluteExecutable("isolation_driver", raw.IsolationDriver); err != nil {
		return Config{}, err
	}
	if raw.GuardExecutable == raw.IsolationDriver {
		return Config{}, fmt.Errorf("%w: guard and isolation driver must be distinct", ErrInvalid)
	}
	if !digestRE.MatchString(raw.DriverSHA256) || !digestRE.MatchString(raw.BaseImageSHA256) {
		return Config{}, fmt.Errorf("%w: isolation driver and base image SHA-256 are required", ErrInvalid)
	}
	if len(raw.Repositories) == 0 || len(raw.Repositories) > 64 {
		return Config{}, fmt.Errorf("%w: repositories must contain 1..64 entries", ErrInvalid)
	}
	durations, err := parseDurations(raw)
	if err != nil {
		return Config{}, err
	}
	policies, err := validatePolicies(raw.Repositories)
	if err != nil {
		return Config{}, err
	}
	return Config{
		APIBaseURL: raw.APIBaseURL, APIVersion: raw.APIVersion,
		StateDir: raw.StateDir, RunnerDirectory: raw.RunnerDirectory,
		GuardExecutable: raw.GuardExecutable, IsolationDriver: raw.IsolationDriver,
		DriverSHA256: raw.DriverSHA256, BaseImageSHA256: raw.BaseImageSHA256,
		QueueWait: durations[0], QueuePoll: durations[1], HTTPTimeout: durations[2],
		JobPollInterval: durations[3], JobBindTimeout: durations[4],
		RunTimeout: durations[5], ShutdownGrace: durations[6], RecoveryTimeout: durations[7],
		Repositories: policies,
	}, nil
}

func pathsOverlap(left, right string) bool {
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		relative, err := filepath.Rel(filepath.Clean(pair[0]), filepath.Clean(pair[1]))
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func validateAPIBase(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: api_base_url must be an HTTPS origin", ErrInvalid)
	}
	if strings.TrimSuffix(u.Path, "/") != "" {
		return fmt.Errorf("%w: api_base_url must not contain a path", ErrInvalid)
	}
	return nil
}

func validateAbsoluteDir(name, value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return fmt.Errorf("%w: %s must be an absolute clean non-root path", ErrInvalid, name)
	}
	return nil
}

func validateAbsoluteExecutable(name, value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return fmt.Errorf("%w: %s must be an absolute clean path", ErrInvalid, name)
	}
	return nil
}

func parseDurations(raw rawConfig) ([8]time.Duration, error) {
	values := []struct {
		name string
		raw  string
		max  time.Duration
	}{
		{"queue_wait", raw.QueueWait, time.Hour}, {"queue_poll", raw.QueuePoll, time.Second},
		{"http_timeout", raw.HTTPTimeout, time.Minute}, {"job_poll_interval", raw.JobPollInterval, time.Minute},
		{"job_bind_timeout", raw.JobBindTimeout, 30 * time.Minute}, {"run_timeout", raw.RunTimeout, 24 * time.Hour},
		{"shutdown_grace", raw.ShutdownGrace, time.Minute}, {"recovery_timeout", raw.RecoveryTimeout, time.Hour},
	}
	var parsed [8]time.Duration
	for index, item := range values {
		value, err := time.ParseDuration(item.raw)
		if err != nil || value <= 0 || value > item.max {
			return parsed, fmt.Errorf("%w: %s duration is invalid", ErrInvalid, item.name)
		}
		parsed[index] = value
	}
	return parsed, nil
}

func validatePolicies(raw []rawRepository) ([]RepositoryPolicy, error) {
	seen := make(map[string]struct{}, len(raw))
	policies := make([]RepositoryPolicy, 0, len(raw))
	for _, item := range raw {
		if _, exists := seen[item.Repository]; exists {
			return nil, fmt.Errorf("%w: duplicate repository policy", ErrInvalid)
		}
		policy := RepositoryPolicy{
			Repository: item.Repository, TrustedRef: item.TrustedRef,
			Workflow: item.Workflow, WorkflowSHA256: item.WorkflowSHA256,
			CandidateRefs: append([]string(nil), item.CandidateRefs...),
			RunnerGroupID: item.RunnerGroupID, JobName: item.JobName,
		}
		if err := validatePolicy(policy); err != nil {
			return nil, err
		}
		seen[item.Repository] = struct{}{}
		policies = append(policies, policy)
	}
	return policies, nil
}

func validatePolicy(policy RepositoryPolicy) error {
	if err := validateRepository(policy.Repository); err != nil {
		return err
	}
	if err := validateRef(policy.TrustedRef); err != nil || !strings.HasPrefix(policy.TrustedRef, "refs/heads/") {
		return fmt.Errorf("%w: trusted_ref must be a safe branch ref", ErrInvalid)
	}
	if !workflowRE.MatchString(policy.Workflow) || !digestRE.MatchString(policy.WorkflowSHA256) {
		return fmt.Errorf("%w: workflow path or SHA-256 is invalid", ErrInvalid)
	}
	if policy.RunnerGroupID <= 0 || !jobNameRE.MatchString(policy.JobName) {
		return fmt.Errorf("%w: runner_group_id or job_name is invalid", ErrInvalid)
	}
	if len(policy.CandidateRefs) == 0 || len(policy.CandidateRefs) > 128 {
		return fmt.Errorf("%w: candidate_refs must contain 1..128 entries", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(policy.CandidateRefs))
	for _, ref := range policy.CandidateRefs {
		if err := validateRef(ref); err != nil {
			return err
		}
		if _, exists := seen[ref]; exists {
			return fmt.Errorf("%w: duplicate candidate ref", ErrInvalid)
		}
		seen[ref] = struct{}{}
	}
	return nil
}

func ValidateRequest(request Request, policy RepositoryPolicy) error {
	if request.Repository != policy.Repository {
		return fmt.Errorf("%w: repository is not allowlisted", ErrInvalid)
	}
	if err := validateRepository(request.Repository); err != nil {
		return err
	}
	if err := validateRef(request.CandidateRef); err != nil {
		return err
	}
	allowed := false
	for _, ref := range policy.CandidateRefs {
		allowed = allowed || ref == request.CandidateRef
	}
	if !allowed || !shaRE.MatchString(request.CandidateSHA) || !idempotencyRE.MatchString(request.Idempotency) {
		return fmt.Errorf("%w: candidate ref, SHA or idempotency key is invalid", ErrInvalid)
	}
	return nil
}

func validateRepository(repository string) error {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || repository != strings.ToLower(repository) {
		return fmt.Errorf("%w: repository must be lowercase owner/repo", ErrInvalid)
	}
	for _, part := range parts {
		if part == "" || len(part) > 100 || !safeName(part) {
			return fmt.Errorf("%w: repository contains an unsafe segment", ErrInvalid)
		}
	}
	return nil
}

func safeName(value string) bool {
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || strings.ContainsRune("._-", char) {
			continue
		}
		return false
	}
	return value[0] != '.' && value[len(value)-1] != '.'
}

func validateRef(ref string) error {
	if len(ref) < 12 || len(ref) > 240 || !strings.HasPrefix(ref, "refs/heads/") {
		return fmt.Errorf("%w: ref must be an exact branch ref", ErrInvalid)
	}
	name := strings.TrimPrefix(ref, "refs/heads/")
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") ||
		strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("%w: ref contains an unsafe sequence", ErrInvalid)
	}
	for _, char := range name {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || strings.ContainsRune("._/-", char) {
			continue
		}
		return fmt.Errorf("%w: ref contains an unsafe character", ErrInvalid)
	}
	return nil
}

func decodeStrictJSON(data []byte, target any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return io.ErrUnexpectedEOF
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func decodeUniqueJSON(data []byte, target any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return io.ErrUnexpectedEOF
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSON(decoder); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func walkJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSON(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkJSON(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}
