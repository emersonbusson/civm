// Package diskaudit provides a read-only disk ownership report for safe,
// known civm VM roots. It never deletes or mutates files.
package diskaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Entry struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type Report struct {
	Entries    []Entry `json:"entries"`
	TotalBytes int64   `json:"total_bytes"`
}

type Options struct {
	HomeDir        string
	RunnerWorkGlob string
	Limit          int
	IncludeSystem  bool
	IncludeDocker  bool

	GlobFn func(pattern string) ([]string, error)
	WalkFn func(root string, fn fs.WalkDirFunc) error
	StatFn func(path string) (fs.FileInfo, error)
	RunFn  func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type rootSpec struct {
	name string
	kind string
	path string
}

func DefaultOptions() Options {
	home, _ := os.UserHomeDir()
	return Options{
		HomeDir:        home,
		RunnerWorkGlob: "/home/*/actions-runner*/_work",
		Limit:          20,
		IncludeSystem:  true,
		IncludeDocker:  true,
		GlobFn:         filepath.Glob,
		WalkFn:         filepath.WalkDir,
		StatFn:         os.Stat,
		RunFn:          defaultRun,
	}
}

func Collect(ctx context.Context, opts Options) Report {
	applyDefaults(&opts)
	entries := make([]Entry, 0, opts.Limit)
	for _, root := range auditRoots(opts) {
		entries = append(entries, measureRoot(ctx, opts, root))
	}
	if opts.IncludeDocker {
		entries = append(entries, dockerEntry(ctx, opts))
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Bytes == entries[j].Bytes {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].Bytes > entries[j].Bytes
	})
	if opts.Limit > 0 && len(entries) > opts.Limit {
		entries = entries[:opts.Limit]
	}
	var total int64
	for _, entry := range entries {
		total += entry.Bytes
	}
	return Report{Entries: entries, TotalBytes: total}
}

func applyDefaults(opts *Options) {
	if opts.HomeDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			opts.HomeDir = home
		}
	}
	if opts.RunnerWorkGlob == "" {
		opts.RunnerWorkGlob = "/home/*/actions-runner*/_work"
	}
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	if opts.GlobFn == nil {
		opts.GlobFn = filepath.Glob
	}
	if opts.WalkFn == nil {
		opts.WalkFn = filepath.WalkDir
	}
	if opts.StatFn == nil {
		opts.StatFn = os.Stat
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
}

func auditRoots(opts Options) []rootSpec {
	seen := map[string]struct{}{}
	var roots []rootSpec
	add := func(name, kind, path string) {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || path == "/" {
			return
		}
		key := kind + "\x00" + path
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		roots = append(roots, rootSpec{name: name, kind: kind, path: path})
	}

	if matches, err := opts.GlobFn(opts.RunnerWorkGlob); err == nil {
		sort.Strings(matches)
		for _, work := range matches {
			add("runner_work", "runner", work)
			add("runner_tool_cache", "runner_cache", filepath.Join(work, "_tool"))
			add("runner_action_cache", "runner_cache", filepath.Join(work, "_actions"))
		}
	}

	if opts.HomeDir != "" {
		home := filepath.Clean(opts.HomeDir)
		add("home_cache", "home_cache", filepath.Join(home, ".cache"))
		add("go_pkg", "go_cache", filepath.Join(home, "go", "pkg"))
		add("codespace", "workspace_clones", filepath.Join(home, "codespace"))
	}
	if opts.IncludeSystem {
		add("var_log", "system", "/var/log")
		add("var_cache", "system", "/var/cache")
	}
	return roots
}

func measureRoot(ctx context.Context, opts Options, root rootSpec) Entry {
	entry := Entry{Name: root.name, Kind: root.kind, Path: root.path, Status: "ok"}
	info, err := opts.StatFn(root.path)
	if err != nil {
		if os.IsNotExist(err) {
			entry.Status = "missing"
			return entry
		}
		entry.Status = "error"
		entry.Error = err.Error()
		return entry
	}
	if !info.IsDir() {
		entry.Bytes = info.Size()
		return entry
	}
	if bytes, err := duSize(ctx, opts, root.path); err == nil {
		entry.Bytes = bytes
		return entry
	} else if ctx.Err() != nil {
		entry.Status = "partial"
		entry.Error = ctx.Err().Error()
		return entry
	}
	var firstErr error
	err = opts.WalkFn(root.path, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() {
			entry.Bytes += info.Size()
		}
		return nil
	})
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		entry.Status = "partial"
		entry.Error = firstErr.Error()
	}
	return entry
}

func duSize(ctx context.Context, opts Options, path string) (int64, error) {
	out, err := opts.RunFn(ctx, "du", "-sb", path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty du output")
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse du output %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n, nil
}

func dockerEntry(ctx context.Context, opts Options) Entry {
	entry := Entry{Name: "docker_reclaimable", Kind: "docker", Path: "(docker daemon)", Status: "ok"}
	out, err := opts.RunFn(ctx, "docker", "system", "df", "--format", "{{.Reclaimable}}")
	if err != nil {
		entry.Status = "error"
		entry.Error = err.Error()
		return entry
	}
	entry.Bytes = parseReclaimable(string(out))
	return entry
}

func RenderJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func RenderText(w io.Writer, r Report) {
	fmt.Fprintf(w, "%-22s %-16s %-12s %-10s %s\n", "NAME", "KIND", "SIZE", "STATUS", "PATH")
	fmt.Fprintln(w, strings.Repeat("-", 86))
	for _, entry := range r.Entries {
		status := entry.Status
		if entry.Error != "" {
			status += ": " + entry.Error
		}
		fmt.Fprintf(w, "%-22s %-16s %-12s %-10s %s\n",
			entry.Name, entry.Kind, FormatBytes(entry.Bytes), truncate(status, 10), entry.Path)
	}
	fmt.Fprintln(w, strings.Repeat("-", 86))
	fmt.Fprintf(w, "%-39s %-12s\n", "TOTAL (listed entries)", FormatBytes(r.TotalBytes))
}

func parseReclaimable(s string) int64 {
	var total int64
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		total += parseHumanBytes(fields[0])
	}
	return total
}

func parseHumanBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mult := int64(1)
	for _, suffix := range []struct {
		raw  string
		mult int64
	}{
		{"GB", 1 << 30},
		{"MB", 1 << 20},
		{"kB", 1 << 10},
		{"KB", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(s, suffix.raw) {
			mult = suffix.mult
			s = strings.TrimSuffix(s, suffix.raw)
			break
		}
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0
	}
	return int64(f * float64(mult))
}

func FormatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(b)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
