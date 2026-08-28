package jitdispatcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigAndValidateRequest(t *testing.T) {
	path := writeConfig(t, validConfigJSON())
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.APIVersion != SupportedAPIVersion || config.RunTimeout != 2*time.Hour || config.RecoveryTimeout != 20*time.Minute {
		t.Fatalf("unexpected config: %+v", config)
	}
	policy, ok := config.Policy("acme/site")
	if !ok {
		t.Fatal("allowlisted policy was not found")
	}
	request := Request{
		Repository: "acme/site", CandidateRef: "refs/heads/feature/safe",
		CandidateSHA: strings.Repeat("a", 40), Idempotency: "request-00000001",
	}
	if err := ValidateRequest(request, policy); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
}

func TestLoadConfigRejectsUnsafeFilesAndJSON(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, path string) string
		body   string
	}{
		{"unknown field", nil, strings.Replace(validConfigJSON(), `"api_base_url"`, `"unknown":true,"api_base_url"`, 1)},
		{"duplicate field", nil, strings.Replace(validConfigJSON(), `"api_version"`, `"api_version":"2026-03-10","api_version"`, 1)},
		{"legacy API", nil, strings.Replace(validConfigJSON(), SupportedAPIVersion, "2022-11-28", 1)},
		{"world readable", func(t *testing.T, path string) string { chmod(t, path, 0o644); return path }, validConfigJSON()},
		{"symlink", func(t *testing.T, path string) string {
			link := path + ".link"
			if err := os.Symlink(path, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, validConfigJSON()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.body)
			if test.mutate != nil {
				path = test.mutate(t, path)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("LoadConfig() accepted unsafe input")
			}
		})
	}
	if _, err := LoadConfig("relative.json"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relative path error = %v", err)
	}
}

func TestValidateRequestRejectsForkSHARefAndReplayKey(t *testing.T) {
	config, err := LoadConfig(writeConfig(t, validConfigJSON()))
	if err != nil {
		t.Fatal(err)
	}
	policy := config.Repositories[0]
	valid := Request{"acme/site", "refs/heads/feature/safe", strings.Repeat("a", 40), "request-00000001"}
	tests := []Request{
		{Repository: "fork/site", CandidateRef: valid.CandidateRef, CandidateSHA: valid.CandidateSHA, Idempotency: valid.Idempotency},
		{Repository: valid.Repository, CandidateRef: "refs/heads/unlisted", CandidateSHA: valid.CandidateSHA, Idempotency: valid.Idempotency},
		{Repository: valid.Repository, CandidateRef: valid.CandidateRef, CandidateSHA: strings.Repeat("A", 40), Idempotency: valid.Idempotency},
		{Repository: valid.Repository, CandidateRef: valid.CandidateRef, CandidateSHA: valid.CandidateSHA[:39], Idempotency: valid.Idempotency},
		{Repository: valid.Repository, CandidateRef: valid.CandidateRef, CandidateSHA: valid.CandidateSHA, Idempotency: "short"},
	}
	for index, request := range tests {
		if err := ValidateRequest(request, policy); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d error = %v", index, err)
		}
	}
}

func TestLoadConfigRejectsInvalidAuthorityFields(t *testing.T) {
	base := validConfigJSON()
	tests := []struct {
		name string
		body string
	}{
		{"HTTP API", strings.Replace(base, "https://api.github.com", "http://api.github.com", 1)},
		{"API path", strings.Replace(base, "https://api.github.com", "https://api.github.com/api", 1)},
		{"root state", strings.Replace(base, "/var/lib/civm/jit-dispatcher", "/", 1)},
		{"relative runner", strings.Replace(base, "/opt/actions-runner", "runner", 1)},
		{"same state and runner", strings.Replace(base, "/opt/actions-runner", "/var/lib/civm/jit-dispatcher", 1)},
		{"runner nested in state", strings.Replace(base, "/opt/actions-runner", "/var/lib/civm/jit-dispatcher/runner", 1)},
		{"state nested in runner", strings.Replace(base, "/var/lib/civm/jit-dispatcher", "/opt/actions-runner/state", 1)},
		{"relative Guard", strings.Replace(base, "/usr/local/bin/guard", "guard", 1)},
		{"same Guard and driver", strings.Replace(base, "/usr/local/libexec/civm-jit-isolation", "/usr/local/bin/guard", 1)},
		{"zero duration", strings.Replace(base, `"30s"`, `"0s"`, 1)},
		{"oversized duration", strings.Replace(base, `"2h"`, `"25h"`, 1)},
		{"no repositories", emptyRepositoriesConfigJSON()},
		{"uppercase repo", strings.Replace(base, "acme/site", "Acme/site", 1)},
		{"trusted tag", strings.Replace(base, "refs/heads/main", "refs/tags/main", 1)},
		{"workflow traversal", strings.Replace(base, ".github/workflows/civm-jit.yml", ".github/workflows/../jit.yml", 1)},
		{"uppercase digest", strings.Replace(base, strings.Repeat("a", 64), strings.Repeat("A", 64), 1)},
		{"zero runner group", strings.Replace(base, `"runner_group_id": 1`, `"runner_group_id": 0`, 1)},
		{"unsafe job", strings.Replace(base, `"job_name": "trusted-jit"`, `"job_name": "bad!job"`, 1)},
		{"no candidate refs", strings.Replace(base, `["refs/heads/feature/safe"]`, `[]`, 1)},
		{"duplicate candidate ref", strings.Replace(base, `["refs/heads/feature/safe"]`, `["refs/heads/feature/safe","refs/heads/feature/safe"]`, 1)},
		{"unsafe candidate ref", strings.Replace(base, "refs/heads/feature/safe", "refs/heads/feature//unsafe", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, test.body)); err == nil {
				t.Fatal("LoadConfig() accepted invalid authority data")
			}
		})
	}
}

func TestLowLevelValidatorsRejectAmbiguousNamesAndJSON(t *testing.T) {
	for _, repository := range []string{"", "acme", "acme/.site", "acme/site.", "acme/si te"} {
		if err := validateRepository(repository); !errors.Is(err, ErrInvalid) {
			t.Errorf("validateRepository(%q) = %v", repository, err)
		}
	}
	for _, ref := range []string{
		"refs/tags/main", "refs/heads/.hidden", "refs/heads/a..b", "refs/heads/a@{b",
		"refs/heads/a.lock", "refs/heads/a?b", "refs/heads/a/",
	} {
		if err := validateRef(ref); !errors.Is(err, ErrInvalid) {
			t.Errorf("validateRef(%q) = %v", ref, err)
		}
	}
	var target map[string]any
	for _, body := range []string{``, `{"a":1}{"b":2}`, `{"a":[{"x":1,"x":2}]}`} {
		if err := decodeUniqueJSON([]byte(body), &target); err == nil {
			t.Errorf("decodeUniqueJSON(%q) accepted invalid JSON", body)
		}
	}
}

func validConfigJSON() string {
	return `{
  "api_base_url": "https://api.github.com",
  "api_version": "2026-03-10",
	  "state_dir": "/var/lib/civm/jit-dispatcher",
	  "runner_directory": "/opt/actions-runner",
	  "guard_executable": "/usr/local/bin/guard",
	  "isolation_driver": "/usr/local/libexec/civm-jit-isolation",
	  "isolation_driver_sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	  "base_image_sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
  "queue_wait": "30s",
  "queue_poll": "100ms",
  "http_timeout": "15s",
  "job_poll_interval": "2s",
  "job_bind_timeout": "2m",
  "run_timeout": "2h",
	  "shutdown_grace": "10s",
	  "recovery_timeout": "20m",
  "repositories": [{
    "repository": "acme/site",
    "trusted_ref": "refs/heads/main",
    "workflow": ".github/workflows/civm-jit.yml",
    "workflow_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "candidate_refs": ["refs/heads/feature/safe"],
    "runner_group_id": 1,
    "job_name": "trusted-jit"
  }]
}`
}

func emptyRepositoriesConfigJSON() string {
	base := validConfigJSON()
	start := strings.Index(base, `  "repositories": [`)
	return base[:start] + "  \"repositories\": []\n}"
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jit.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
