// Package drift compares the locally-pinned RunnerImageSpec against the
// upstream actions/runner-images Ubuntu2404-Readme.md to detect when the
// GitHub-hosted runner image has moved ahead of our pins.
package drift

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/specs"
)

// Diff is one tool whose pinned version differs from upstream.
type Diff struct {
	Tool     string
	Pinned   string
	Upstream string
	Status   Status
}

// Status of a single diff.
type Status int

const (
	StatusInSync Status = iota
	StatusBehind
	StatusAhead
	StatusUpstreamMissing
)

func (s Status) String() string {
	switch s {
	case StatusInSync:
		return "in-sync"
	case StatusBehind:
		return "behind"
	case StatusAhead:
		return "ahead"
	case StatusUpstreamMissing:
		return "missing-upstream"
	}
	return "?"
}

// Report is the full drift report.
type Report struct {
	UpstreamURL string
	Generated   time.Time
	Diffs       []Diff
	UpstreamRaw string // raw markdown fetched (truncated to first 16KB for traceability)
}

// HasBehind returns true if any tool is behind upstream. "Ahead" e
// "missing-upstream" nao contam — apenas behind real e problema.
func (r Report) HasBehind() bool {
	for _, d := range r.Diffs {
		if d.Status == StatusBehind {
			return true
		}
	}
	return false
}

// HasAhead reports whether any pinned tool is newer than upstream.
func (r Report) HasAhead() bool {
	for _, d := range r.Diffs {
		if d.Status == StatusAhead {
			return true
		}
	}
	return false
}

// Options control how drift is computed.
type Options struct {
	Spec        specs.RunnerImageSpec
	UpstreamURL string
	Timeout     time.Duration
	Fetcher     func(ctx context.Context, url string) (string, error)
}

// DefaultOptions wires the production fetcher.
func DefaultOptions() Options {
	return Options{
		Spec:        specs.Ubuntu2404(),
		UpstreamURL: specs.Ubuntu2404().UpstreamURL,
		Timeout:     15 * time.Second,
		Fetcher:     httpFetch,
	}
}

// Detect fetches upstream and computes the diff.
func Detect(ctx context.Context, opts Options) (Report, error) {
	if opts.Fetcher == nil {
		opts.Fetcher = httpFetch
	}
	if opts.Timeout == 0 {
		opts.Timeout = 15 * time.Second
	}
	url := opts.UpstreamURL
	if !strings.Contains(url, "raw.githubusercontent.com") {
		url = toRawURL(url)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	body, err := opts.Fetcher(fetchCtx, url)
	if err != nil {
		return Report{}, fmt.Errorf("fetch upstream %s: %w", url, err)
	}
	upstreamVersions := parseUpstreamVersions(body)
	report := Report{
		UpstreamURL: url,
		Generated:   time.Now().UTC(),
		UpstreamRaw: truncate(body, 16*1024),
	}
	for _, t := range opts.Spec.Tools {
		key := normalizeName(t.Name)
		upstreamList, ok := upstreamVersions[key]
		upstreamCSV := strings.Join(upstreamList, ", ")
		switch {
		case !ok || len(upstreamList) == 0:
			report.Diffs = append(report.Diffs, Diff{
				Tool:     t.Name,
				Pinned:   t.Preferred(),
				Upstream: "(not parsed)",
				Status:   StatusUpstreamMissing,
			})
		case contains(upstreamList, t.Preferred()):
			report.Diffs = append(report.Diffs, Diff{
				Tool:     t.Name,
				Pinned:   t.Preferred(),
				Upstream: upstreamCSV,
				Status:   StatusInSync,
			})
		case isSemverAhead(t.Preferred(), upstreamList):
			report.Diffs = append(report.Diffs, Diff{
				Tool:     t.Name,
				Pinned:   t.Preferred(),
				Upstream: upstreamCSV,
				Status:   StatusAhead,
			})
		default:
			report.Diffs = append(report.Diffs, Diff{
				Tool:     t.Name,
				Pinned:   t.Preferred(),
				Upstream: upstreamCSV,
				Status:   StatusBehind,
			})
		}
	}
	sort.Slice(report.Diffs, func(i, j int) bool {
		return report.Diffs[i].Tool < report.Diffs[j].Tool
	})
	return report, nil
}

// Render writes a fixed-width table.
func (r Report) Render(w io.Writer) {
	fmt.Fprintf(w, "Drift report (gerado %s)\n", r.Generated.Format(time.RFC3339))
	fmt.Fprintf(w, "Upstream: %s\n\n", r.UpstreamURL)
	fmt.Fprintf(w, "%-16s %-14s %-14s %s\n", "TOOL", "PINNED", "UPSTREAM", "STATUS")
	fmt.Fprintln(w, strings.Repeat("-", 60))
	for _, d := range r.Diffs {
		fmt.Fprintf(w, "%-16s %-14s %-14s %s\n", d.Tool, d.Pinned, d.Upstream, d.Status)
	}
	fmt.Fprintln(w, strings.Repeat("-", 60))
	switch {
	case r.HasBehind():
		fmt.Fprintln(w, "ATENCAO: pelo menos uma ferramenta esta atras do upstream.")
		fmt.Fprintln(w, "Editar internal/specs/specs.go com as versoes upstream + commit.")
	case r.HasAhead():
		fmt.Fprintln(w, "INFO: pelo menos uma ferramenta esta a frente do upstream (VM atualizada).")
		fmt.Fprintln(w, "Aguarde upstream actions/runner-images publicar nova imagem.")
	default:
		fmt.Fprintln(w, "OK: todas as ferramentas estao em sync com upstream.")
	}
}

// ---- parsing ----

// linePatterns identifies the line that contains versions for each tool.
// We then extract every semver-like substring from that line.
//
// Upstream actions/runner-images formats observed:
//   - "- Node.js 20.20.2"
//   - "- Docker Client 28.0.4"
//   - "- GitHub CLI 2.89.0"
//   - "- Git 2.53.0"
//   - "- jq 1.7"
//   - "- yq 4.52.5"
//   - "**Node.js:** 20.20.2 (system)" (older format)
//   - "| jq | 1.7.1-3ubuntu0.24.04.1 |" (apt table)
//
// Patterns accept both bullet and bold-prefix forms; "Go" is special
// because it lives under a section heading "#### Go" with a table below.
var linePatterns = []struct {
	tool  string
	regex *regexp.Regexp
}{
	{"go", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*Go(?:lang)?[\s\*:|]+([0-9][^\n]*)$`)},
	{"node", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*Node\.?js[\s\*:|]+([0-9][^\n]*)$`)},
	{"python", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*Python[\s\*:|]+([0-9][^\n]*)$`)},
	{"docker-compose", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*Docker[\s-]*Compose(?:\s*v?2)?[\s\*:|]+([0-9][^\n]*)$`)},
	{"docker", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*Docker(?:\s+(?:Client|Server))?[\s\*:|]+([0-9][^\n]*)$`)},
	{"gh", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*GitHub\s*CLI(?:\s*\(gh\))?[\s\*:|]+([0-9][^\n]*)$`)},
	{"git", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*Git[\s\*:|]+([0-9][^\n]*)$`)},
	{"jq", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*jq[\s\*:|]+([0-9][^\n]*)$`)},
	{"yq", regexp.MustCompile(`(?im)^(?:[-\s|*]*)?\*?\*?\s*yq[\s\*:|]+([0-9][^\n]*)$`)},
}

var semverRegex = regexp.MustCompile(`[0-9]+\.[0-9]+(?:\.[0-9]+)?`)

func parseUpstreamVersions(body string) map[string][]string {
	out := map[string][]string{}
	// First pass: tool-specific line patterns (single match each).
	for _, p := range linePatterns {
		line := p.regex.FindString(body)
		if line == "" {
			continue
		}
		matches := semverRegex.FindAllString(line, -1)
		if len(matches) == 0 {
			continue
		}
		// Skip the docker-compose match for the docker key (avoid 2.38.2 leaking
		// into the docker tool).
		if p.tool == "docker" && len(out["docker-compose"]) > 0 {
			// docker-compose was already matched; ensure docker doesn't accidentally
			// re-capture it. Since regexes target distinct lines, this is normally fine.
			_ = matches
		}
		out[p.tool] = uniqueStrings(matches)
	}
	// Second pass: aggregate Go/Node/Python from section headings if available.
	// Real upstream has "#### Go" followed by a table with version columns.
	for _, sec := range []string{"go", "node", "python"} {
		extra := parseSectionVersions(body, sec)
		if len(extra) > 0 {
			out[sec] = uniqueStrings(append(out[sec], extra...))
		}
	}
	return out
}

// parseSectionVersions extracts every version token after a "#### Tool"
// heading until the next section.
func parseSectionVersions(body, tool string) []string {
	headRe := regexp.MustCompile(`(?im)^####\s+` + regexp.QuoteMeta(displayName(tool)) + `\s*$`)
	loc := headRe.FindStringIndex(body)
	if loc == nil {
		return nil
	}
	rest := body[loc[1]:]
	endRe := regexp.MustCompile(`(?m)^####\s+\w`)
	endLoc := endRe.FindStringIndex(rest)
	section := rest
	if endLoc != nil {
		section = rest[:endLoc[0]]
	}
	return semverRegex.FindAllString(section, -1)
}

// displayName maps internal keys to upstream heading names.
func displayName(tool string) string {
	switch tool {
	case "go":
		return "Go"
	case "node":
		return "Node.js"
	case "python":
		return "Python"
	}
	return tool
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// isSemverAhead returns true when pinned is semver-greater than every
// version in upstreamList. Used to detect "VM ahead of GitHub-hosted",
// which is OK (not behind, not in-sync — informational).
func isSemverAhead(pinned string, upstreamList []string) bool {
	pParts := splitVersion(pinned)
	if len(pParts) == 0 {
		return false
	}
	for _, u := range upstreamList {
		uParts := splitVersion(u)
		if compareVersion(pParts, uParts) <= 0 {
			return false
		}
	}
	return true
}

func splitVersion(s string) []int {
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	return out
}

func compareVersion(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// toRawURL converts a github.com/.../blob/... URL to raw.githubusercontent.com.
func toRawURL(u string) string {
	u = strings.Replace(u, "https://github.com/", "https://raw.githubusercontent.com/", 1)
	u = strings.Replace(u, "/blob/", "/", 1)
	return u
}

// httpFetch is the production HTTP fetcher.
func httpFetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "civmctl-drift/1.0")
	req.Header.Set("Accept", "text/plain, text/markdown")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
