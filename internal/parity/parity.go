// Package parity compares tools installed on the VM with the civm
// ubuntu-latest parity spec.
package parity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/specs"
)

type Status string

const (
	StatusInSync  Status = "in-sync"
	StatusCompat  Status = "compatible"
	StatusAhead   Status = "ahead"
	StatusBehind  Status = "behind"
	StatusMissing Status = "missing"
	StatusUnknown Status = "unknown"
)

type Options struct {
	Spec    specs.RunnerImageSpec
	Timeout time.Duration
	RunFn   func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ToolCheck struct {
	Tool     string   `json:"tool"`
	Command  string   `json:"command"`
	Expected []string `json:"expected"`
	Actual   string   `json:"actual"`
	Status   Status   `json:"status"`
	Error    string   `json:"error,omitempty"`
}

type Report struct {
	OSDistro     string      `json:"os_distro"`
	OSVersion    string      `json:"os_version"`
	ImageVersion string      `json:"image_version"`
	Checks       []ToolCheck `json:"checks"`
	Exit         int         `json:"exit"`
}

type commandSpec struct {
	name string
	args []string
}

var versionRe = regexp.MustCompile(`v?([0-9]+[.][0-9]+(?:[.][0-9]+)?)`)

func Check(ctx context.Context, opts Options) Report {
	if opts.Spec.OSDistro == "" {
		opts.Spec = specs.Ubuntu2404()
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	report := Report{
		OSDistro:     opts.Spec.OSDistro,
		OSVersion:    opts.Spec.OSVersion,
		ImageVersion: opts.Spec.ImageVersion,
		Checks:       make([]ToolCheck, 0, len(opts.Spec.Tools)),
	}
	for _, tool := range opts.Spec.Tools {
		cmd := commandForTool(tool.Name)
		c := ToolCheck{
			Tool:     tool.Name,
			Command:  strings.TrimSpace(cmd.name + " " + strings.Join(cmd.args, " ")),
			Expected: append([]string(nil), tool.Versions...),
		}
		out, err := opts.RunFn(checkCtx, cmd.name, cmd.args...)
		if err != nil {
			c.Status = StatusMissing
			c.Error = err.Error()
			report.Exit = 1
			report.Checks = append(report.Checks, c)
			continue
		}
		actual := parseVersion(string(out))
		c.Actual = actual
		switch {
		case actual == "":
			c.Status = StatusUnknown
			report.Exit = 1
		case containsVersion(tool.Versions, actual):
			c.Status = StatusInSync
		case compatibleVersion(tool.Name, actual, tool.Versions):
			c.Status = StatusCompat
		case versionAhead(actual, tool.Versions):
			c.Status = StatusAhead
		default:
			c.Status = StatusBehind
			report.Exit = 1
		}
		report.Checks = append(report.Checks, c)
	}
	return report
}

func (r Report) Render(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Paridade VM vs ubuntu-latest: %s %s (image %s)\n\n", r.OSDistro, r.OSVersion, r.ImageVersion)
	_, _ = fmt.Fprintf(w, "%-16s %-12s %-26s %s\n", "Tool", "Status", "Atual", "Esperado")
	_, _ = fmt.Fprintln(w, strings.Repeat("-", 82))
	for _, c := range r.Checks {
		actual := c.Actual
		if actual == "" {
			actual = "-"
		}
		_, _ = fmt.Fprintf(w, "%-16s %-12s %-26s %s\n", c.Tool, c.Status, actual, strings.Join(c.Expected, ", "))
		if c.Error != "" {
			_, _ = fmt.Fprintf(w, "  erro: %s\n", c.Error)
		}
	}
	_, _ = fmt.Fprintln(w, strings.Repeat("-", 82))
	switch r.Exit {
	case 0:
		_, _ = fmt.Fprintln(w, "OK: VM em paridade aceitavel com os pins.")
	default:
		_, _ = fmt.Fprintln(w, "ATENCAO: VM tem ferramenta ausente ou atrasada frente aos pins.")
	}
}

func (r Report) RenderString() string {
	var buf bytes.Buffer
	r.Render(&buf)
	return buf.String()
}

func (r Report) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func commandForTool(tool string) commandSpec {
	switch tool {
	case "go":
		return commandSpec{name: "go", args: []string{"version"}}
	case "node":
		return commandSpec{name: "node", args: []string{"--version"}}
	case "python":
		return commandSpec{name: "python3", args: []string{"--version"}}
	case "docker":
		return commandSpec{name: "docker", args: []string{"--version"}}
	case "docker-compose":
		return commandSpec{name: "docker", args: []string{"compose", "version"}}
	case "gh":
		return commandSpec{name: "gh", args: []string{"--version"}}
	case "git":
		return commandSpec{name: "git", args: []string{"--version"}}
	case "jq":
		return commandSpec{name: "jq", args: []string{"--version"}}
	case "yq":
		return commandSpec{name: "yq", args: []string{"--version"}}
	default:
		return commandSpec{name: tool, args: []string{"--version"}}
	}
}

func parseVersion(out string) string {
	m := versionRe.FindStringSubmatch(out)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func containsVersion(values []string, actual string) bool {
	for _, v := range values {
		if v == actual {
			return true
		}
	}
	return false
}

func compatibleVersion(tool, actual string, expected []string) bool {
	switch tool {
	case "python":
		return matchesPrefixParts(actual, expected, 2)
	case "git":
		return matchesPrefixParts(actual, expected, 1)
	default:
		return false
	}
}

func matchesPrefixParts(actual string, expected []string, parts int) bool {
	actualParts := semverParts(actual)
	for _, value := range expected {
		expectedParts := semverParts(value)
		match := true
		for i := 0; i < parts; i++ {
			if actualParts[i] != expectedParts[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func versionAhead(actual string, expected []string) bool {
	if len(expected) == 0 {
		return false
	}
	for _, v := range expected {
		if compareSemver(actual, v) <= 0 {
			return false
		}
	}
	return true
}

func compareSemver(a, b string) int {
	aa := semverParts(a)
	bb := semverParts(b)
	for i := 0; i < 3; i++ {
		switch {
		case aa[i] > bb[i]:
			return 1
		case aa[i] < bb[i]:
			return -1
		}
	}
	return 0
}

func semverParts(v string) [3]int {
	parts := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		n, _ := strconv.Atoi(parts[i])
		out[i] = n
	}
	return out
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
