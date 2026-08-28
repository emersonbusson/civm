package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Status of a single runner systemd service.
type Status struct {
	UnitName    string `json:"unit_name"`
	Repo        string `json:"repo"`          // extraído do unit name (owner-repo)
	Org         string `json:"org,omitempty"` // extraído de .runner quando o runner é org-level
	Name        string `json:"name"`          // ex: civm-cmpx
	LoadState   string `json:"load_state"`    // loaded, not-found, etc
	ActiveState string `json:"active_state"`  // active, inactive, failed
	SubState    string `json:"sub_state"`     // running, dead, etc
	Description string `json:"description"`
	// WorkingDirectory is the runner service WorkingDirectory (systemctl show),
	// populated by the watchdog enrich step. The hook's WorkRoot lives under it,
	// so the watchdog maps a broken-runner sentinel to this unit (RF-6/ITEM-10).
	WorkingDirectory string `json:"working_directory,omitempty"`
}

// ListOptions control runner listing.
type ListOptions struct {
	RunFn func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultListOptions returns sane defaults.
func DefaultListOptions() ListOptions {
	return ListOptions{RunFn: defaultRun}
}

// List parses `systemctl list-units actions.runner.*` output into
// structured records. Returns empty slice (not nil) when no runners
// found or systemctl indisponível — never returns error for absent state.
func List(ctx context.Context, opts ListOptions) ([]Status, error) {
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	out, err := opts.RunFn(ctx, "systemctl",
		"list-units", "--type=service", "--no-pager", "--no-legend",
		"--all", "actions.runner.*")
	if err != nil {
		return []Status{}, nil
	}
	return parseSystemctlList(string(out)), nil
}

// parseSystemctlList parses lines like:
//
//	"  actions.runner.OWNER-REPO.NAME.service  loaded active running GitHub Actions Runner (...)"
//
// Empty lines are skipped. Lines without 5+ fields are skipped.
func parseSystemctlList(stdout string) []Status {
	var out []Status
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		// systemctl prefixes inactive/failed units with "●" or "○"
		line = strings.TrimLeft(line, "●○ ")
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		s := Status{
			UnitName:    fields[0],
			LoadState:   fields[1],
			ActiveState: fields[2],
			SubState:    fields[3],
		}
		if len(fields) >= 5 {
			s.Description = strings.Join(fields[4:], " ")
		}
		s.Repo, s.Name = parseRunnerUnit(s.UnitName)
		out = append(out, s)
	}
	return out
}

// parseRunnerUnit extracts repo and runner-name from a unit like
// "actions.runner.owner-repo.runner-1.service".
func parseRunnerUnit(unit string) (repo, name string) {
	const prefix = "actions.runner."
	const suffix = ".service"
	if !strings.HasPrefix(unit, prefix) {
		return "", ""
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(unit, prefix), suffix)
	// rest = "owner-repo.runner-1"
	idx := strings.LastIndex(rest, ".")
	if idx == -1 {
		return rest, ""
	}
	repoSegment := rest[:idx]
	name = rest[idx+1:]
	// repoSegment "owner-repo" becomes "owner/repo".
	dashIdx := strings.Index(repoSegment, "-")
	if dashIdx == -1 {
		return repoSegment, name
	}
	repo = repoSegment[:dashIdx] + "/" + repoSegment[dashIdx+1:]
	return repo, name
}

// RenderListTable writes a fixed-width table.
func RenderListTable(items []Status, w io.Writer) {
	if len(items) == 0 {
		fmt.Fprintln(w, "Nenhum runner systemd ativo (actions.runner.*).")
		fmt.Fprintln(w, "Para registrar: civmctl runner add --repo=owner/repo --token=... --short=...")
		return
	}
	fmt.Fprintf(w, "%-50s %-12s %-10s %s\n", "RUNNER", "REPO", "STATE", "NAME")
	fmt.Fprintln(w, strings.Repeat("-", 100))
	for _, s := range items {
		state := s.ActiveState
		if s.SubState != "" && s.SubState != "running" && s.SubState != "dead" {
			state = state + "/" + s.SubState
		}
		fmt.Fprintf(w, "%-50s %-12s %-10s %s\n",
			truncate(s.UnitName, 50), truncate(s.Repo, 12), state, s.Name)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Total: %d runner(s)\n", len(items))
}

// RenderListJSON emits machine-readable output.
func RenderListJSON(items []Status, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"runners": items,
		"count":   len(items),
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 2 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
