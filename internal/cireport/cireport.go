// Package cireport posts a commit status to GitHub via the Statuses API.
// Designed for "manual reporter" use cases when the automatic Camada 1
// (router workflow) cannot run — ex.: peer is offline, billing-block
// AND civm runner offline. Each peer can invoke from its own
// pre-push gate runner.
//
// Stdlib-only (uses gh CLI via os/exec, no SDK).
package cireport

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// State of a commit status.
type State string

const (
	StateSuccess State = "success"
	StateFailure State = "failure"
	StatePending State = "pending"
	StateError   State = "error"
)

// Validate State string.
func (s State) Valid() bool {
	switch s {
	case StateSuccess, StateFailure, StatePending, StateError:
		return true
	}
	return false
}

// Options control the report.
type Options struct {
	Repo        string // "owner/repo"
	SHA         string // commit SHA (full or 40-char)
	State       State  // success, failure, pending, error
	Context     string // ex: "civm fallback"
	Description string // breve descricao (<=140 chars)
	TargetURL   string // optional URL to detalhes (build log, etc)
	RunFn       func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultOptions returns sane defaults.
func DefaultOptions() Options {
	return Options{
		Context: "civm fallback",
		RunFn:   defaultRun,
	}
}

// Post posts the commit status via gh api. Returns the raw API response
// (or error). Idempotente: GitHub aceita repostar com mesmo Context +
// State diferentes (sobrepoe).
func Post(ctx context.Context, opts Options) ([]byte, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	endpoint := fmt.Sprintf("/repos/%s/statuses/%s", opts.Repo, opts.SHA)
	args := []string{
		"api", "-X", "POST", endpoint,
		"-f", "state=" + string(opts.State),
		"-f", "context=" + opts.Context,
	}
	if opts.Description != "" {
		args = append(args, "-f", "description="+truncate(opts.Description, 140))
	}
	if opts.TargetURL != "" {
		args = append(args, "-f", "target_url="+opts.TargetURL)
	}
	out, err := opts.RunFn(ctx, "gh", args...)
	if err != nil {
		return out, fmt.Errorf("gh api POST: %w", err)
	}
	return out, nil
}

func validateOptions(opts Options) error {
	if opts.Repo == "" {
		return fmt.Errorf("--repo obrigatorio (formato: owner/repo)")
	}
	if !strings.Contains(opts.Repo, "/") {
		return fmt.Errorf("--repo deve ter formato owner/repo, got %q", opts.Repo)
	}
	if opts.SHA == "" {
		return fmt.Errorf("--sha obrigatorio")
	}
	if !opts.State.Valid() {
		return fmt.Errorf("--state invalido %q (valores: success, failure, pending, error)", opts.State)
	}
	if opts.Context == "" {
		return fmt.Errorf("--context obrigatorio (ex: 'civm fallback')")
	}
	return nil
}

// Render writes a confirmation summary.
func Render(opts Options, response []byte, w io.Writer) {
	fmt.Fprintf(w, "Repo:        %s\n", opts.Repo)
	fmt.Fprintf(w, "SHA:         %s\n", opts.SHA)
	fmt.Fprintf(w, "State:       %s\n", opts.State)
	fmt.Fprintf(w, "Context:     %s\n", opts.Context)
	if opts.Description != "" {
		fmt.Fprintf(w, "Description: %s\n", opts.Description)
	}
	if opts.TargetURL != "" {
		fmt.Fprintf(w, "Target URL:  %s\n", opts.TargetURL)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Status posted via GitHub Statuses API.")
	fmt.Fprintln(w, "Verifique em: https://github.com/"+opts.Repo+"/commit/"+opts.SHA)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 4 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
