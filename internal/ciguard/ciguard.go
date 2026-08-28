// Package ciguard performs a read-only lint of a consumer repository's
// docker-compose files and GitHub Actions workflows against the civm
// multi-project isolation invariants. It never executes file content; it only
// reads files under RepoRoot and reports findings.
//
// Discipline: #5 Availability heuristic (disciplines/KAHNEMAN-DISCIPLINES.md) —
// surface the collision modes a reviewer would otherwise forget, before they
// reach a shared runner.
package ciguard

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Severity classifies a finding.
type Severity string

const (
	// SeverityError fails ci-guard in enforce mode when not waived.
	SeverityError Severity = "error"
	// SeverityWarn never fails enforce; report-only signal.
	SeverityWarn Severity = "warn"
)

// Mode controls whether Scan results gate the exit code in the CLI.
const (
	ModeReport  = "report"
	ModeEnforce = "enforce"
)

// Rule identifiers. Each rule is documented in the consumer runbook.
const (
	RuleContainerName  = "R1-container-name"
	RuleStaticHostPort = "R2-static-host-port"
	RuleMissingProject = "R3-missing-project-name"
	RuleUnlockedHeavy  = "R4-unlocked-docker-heavy"
	RuleOrphanWaiver   = "orphan-waiver"
)

const (
	remediationContainerName  = "remova container_name: nomes fixos impedem co-residencia entre runners"
	remediationStaticHostPort = "use ${CIVM_PORT_BASE}+N ou porta ephemeral em vez de host-port estatica"
	remediationMissingProject = "passe -p/--project-name ou exporte COMPOSE_PROJECT_NAME no escopo do step"
	remediationUnlockedHeavy  = "envolva o mesmo comando em civmctl admit --weight heavy --exclusive docker --exec; flock repo-local nao difere a cleanup"
	remediationOrphanWaiver   = "remova o waiver: a regra citada nao casou nenhum finding na proxima linha"
)

// waiverPrefix is the inline comment that suppresses one rule on the next
// significant line: "# civm:ci-guard-allow <rule> <motivo>".
const waiverPrefix = "civm:ci-guard-allow"

// Finding is a single rule violation or warning.
type Finding struct {
	File        string   `json:"file"`
	Line        int      `json:"line"`
	Rule        string   `json:"rule"`
	Severity    Severity `json:"severity"`
	Message     string   `json:"message"`
	Remediation string   `json:"remediation"`
}

// Result is the full ci-guard outcome.
type Result struct {
	Findings   []Finding `json:"findings"`
	Violations int       `json:"violations"`
	Mode       string    `json:"mode"`
}

// Options configures Scan. Every filesystem access is injected so unit tests
// run without touching the real working tree.
type Options struct {
	RepoRoot   string
	Mode       string
	GlobFn     func(pattern string) ([]string, error)
	ReadFileFn func(path string) ([]byte, error)
	// WalkFn is optional; when nil, Scan globs the known infra/workflow paths.
	WalkFn func(root string, fn fs.WalkDirFunc) error
}

// DefaultOptions wires the production filesystem functions.
func DefaultOptions(repoRoot string) Options {
	if strings.TrimSpace(repoRoot) == "" {
		repoRoot = "."
	}
	return Options{
		RepoRoot:   repoRoot,
		Mode:       ModeReport,
		GlobFn:     filepath.Glob,
		ReadFileFn: os.ReadFile,
		WalkFn:     filepath.WalkDir,
	}
}

func applyDefaults(opts *Options) {
	if strings.TrimSpace(opts.RepoRoot) == "" {
		opts.RepoRoot = "."
	}
	if opts.Mode == "" {
		opts.Mode = ModeReport
	}
	if opts.GlobFn == nil {
		opts.GlobFn = filepath.Glob
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = os.ReadFile
	}
	if opts.WalkFn == nil {
		opts.WalkFn = filepath.WalkDir
	}
}

// Scan reads compose files under infra/ and workflows under .github/workflows/,
// applies rules R1-R4, honours inline waivers, and returns the findings plus
// the count of non-waived error-severity violations.
func Scan(opts Options) (Result, error) {
	applyDefaults(&opts)
	result := Result{Mode: opts.Mode}

	composeFiles, err := composePaths(opts)
	if err != nil {
		return result, fmt.Errorf("locate compose files: %w", err)
	}
	workflowFiles, err := workflowPaths(opts)
	if err != nil {
		return result, fmt.Errorf("locate workflow files: %w", err)
	}

	for _, path := range composeFiles {
		findings, scanErr := scanComposeFile(opts, path)
		if scanErr != nil {
			return result, fmt.Errorf("scan compose %s: %w", path, scanErr)
		}
		result.Findings = append(result.Findings, findings...)
	}
	for _, path := range workflowFiles {
		findings, scanErr := scanWorkflowFile(opts, path)
		if scanErr != nil {
			return result, fmt.Errorf("scan workflow %s: %w", path, scanErr)
		}
		result.Findings = append(result.Findings, findings...)
	}

	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].File == result.Findings[j].File {
			return result.Findings[i].Line < result.Findings[j].Line
		}
		return result.Findings[i].File < result.Findings[j].File
	})
	result.Violations = countViolations(result.Findings)
	return result, nil
}

func countViolations(findings []Finding) int {
	count := 0
	for _, f := range findings {
		if f.Severity == SeverityError {
			count++
		}
	}
	return count
}

// composePaths resolves docker-compose*.yml/.yaml under <repo>/infra (recursive
// when WalkFn is set) plus the repo root level, deduplicated and sorted.
func composePaths(opts Options) ([]string, error) {
	seen := map[string]struct{}{}
	var paths []string
	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	globPatterns := []string{
		filepath.Join(opts.RepoRoot, "docker-compose*.yml"),
		filepath.Join(opts.RepoRoot, "docker-compose*.yaml"),
		filepath.Join(opts.RepoRoot, "infra", "docker-compose*.yml"),
		filepath.Join(opts.RepoRoot, "infra", "docker-compose*.yaml"),
	}
	for _, pattern := range globPatterns {
		matches, err := opts.GlobFn(pattern)
		if err != nil {
			return nil, err
		}
		sort.Strings(matches)
		for _, m := range matches {
			add(m)
		}
	}

	infraRoot := filepath.Join(opts.RepoRoot, "infra")
	walkErr := opts.WalkFn(infraRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if isComposeFile(d.Name()) {
			add(path)
		}
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, walkErr
	}
	sort.Strings(paths)
	return paths, nil
}

// workflowPaths resolves .github/workflows/*.yml/.yaml, sorted.
func workflowPaths(opts Options) ([]string, error) {
	var paths []string
	for _, pattern := range []string{
		filepath.Join(opts.RepoRoot, ".github", "workflows", "*.yml"),
		filepath.Join(opts.RepoRoot, ".github", "workflows", "*.yaml"),
	} {
		matches, err := opts.GlobFn(pattern)
		if err != nil {
			return nil, err
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	return paths, nil
}

func isComposeFile(name string) bool {
	if !strings.HasPrefix(name, "docker-compose") {
		return false
	}
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

var (
	containerNameRe = regexp.MustCompile(`^\s*container_name\s*:`)
	// staticHostPortRe captures a quoted or bare "HOST:CONTAINER" ports entry
	// where HOST is a literal integer. Env-interpolated (${...}) or single-port
	// forms are intentionally not matched.
	staticHostPortRe = regexp.MustCompile(`^\s*-\s*["']?(\d+):\d+(?:/\w+)?["']?\s*$`)
	composeInvokeRe  = regexp.MustCompile(`docker[\s-]compose\b|docker-compose\b`)
	// dockerHeavyUpRe casa passos docker-heavy: `docker compose up`, `--build`,
	// `make up`/`make up-local*` e `devctl ci up`. O e2e-tenant-isolation.yml chama
	// `go run .../devctl ci up core`, que por dentro roda `make up-local-smoke`
	// — docker-heavy real, mas invisivel aos demais padroes. O `\b` apos `up`
	// evita falso-positivo em `make update`/`make upstream`. Ver ADR-107.
	dockerHeavyUpRe = regexp.MustCompile(`docker[\s-]compose\b.*\bup\b|--build\b|\bmake\s+up\b|\bdevctl\b.*\bci\s+up\b`)
	projectNameRe   = regexp.MustCompile(`-p\b|--project-name\b|COMPOSE_PROJECT_NAME`)
	// legacyLockWrapRe keeps existing consumers covered while they migrate to
	// admission. A repository-local flock is deliberately not accepted: cleanup
	// cannot observe it, so it may prune the shared daemon mid-extract.
	legacyLockWrapRe   = regexp.MustCompile(`civmctl\s+lock\b`)
	legacyLockSubcmdRe = regexp.MustCompile(`^\s*(?:acquire|release)\b`)
	admitWrapRe        = regexp.MustCompile(`civmctl\s+admit\b`)
	heavyWeightRe      = regexp.MustCompile(`--weight(?:=|\s+)heavy(?:\s|$)`)
	dockerExclusiveRe  = regexp.MustCompile(`--exclusive(?:=|\s+)docker(?:\s|$)`)
	execWrapRe         = regexp.MustCompile(`--exec\b`)
	payloadSeparatorRe = regexp.MustCompile(`(?:^|\s)--(?:\s|$)`)
	shellControlRe     = regexp.MustCompile(`&&|\|\||;|\|`)
)

type waiver struct {
	rule    string
	matched bool
}

// scanComposeFile applies R1 (container_name) and R2 (static host port).
func scanComposeFile(opts Options, path string) ([]Finding, error) {
	lines, err := readLines(opts, path)
	if err != nil {
		return nil, err
	}
	waivers := map[int]*waiver{}
	var findings []Finding
	for idx, raw := range lines {
		lineNo := idx + 1
		if w := parseWaiver(raw); w != nil {
			if target := nextSignificantLine(lines, idx); target >= 0 {
				waivers[target] = w
			} else {
				waivers[-1] = w // orphan placeholder; resolved at end
			}
			continue
		}
		findings = appendIfActive(findings, waivers, Finding{
			File: path, Line: lineNo, Rule: RuleContainerName, Severity: SeverityError,
			Message: "container_name fixo impede co-residencia entre runners", Remediation: remediationContainerName,
		}, idx, containerNameRe.MatchString(raw))
		findings = appendIfActive(findings, waivers, Finding{
			File: path, Line: lineNo, Rule: RuleStaticHostPort, Severity: SeverityError,
			Message: "host-port estatica colide entre projetos concorrentes", Remediation: remediationStaticHostPort,
		}, idx, staticHostPortRe.MatchString(raw))
	}
	findings = append(findings, orphanWaiverFindings(path, lines, waivers)...)
	return findings, nil
}

// scanWorkflowFile applies R3 (compose without project name) and R4 (docker-heavy
// step without a central wrapper, warn-only).
func scanWorkflowFile(opts Options, path string) ([]Finding, error) {
	lines, err := readLines(opts, path)
	if err != nil {
		return nil, err
	}
	hasProjectScope := fileHasProjectScope(lines)
	waivers := map[int]*waiver{}
	var findings []Finding
	for idx, raw := range lines {
		lineNo := idx + 1
		if w := parseWaiver(raw); w != nil {
			if target := nextSignificantLine(lines, idx); target >= 0 {
				waivers[target] = w
			} else {
				waivers[-1] = w
			}
			continue
		}
		missingProject := composeInvokeRe.MatchString(raw) && !projectNameRe.MatchString(raw) && !hasProjectScope
		findings = appendIfActive(findings, waivers, Finding{
			File: path, Line: lineNo, Rule: RuleMissingProject, Severity: SeverityError,
			Message: "docker compose sem project-name colide COMPOSE_PROJECT_NAME entre runners", Remediation: remediationMissingProject,
		}, idx, missingProject)
		unlockedHeavy := dockerHeavyUpRe.MatchString(raw) && !protectsDockerHeavy(raw)
		findings = appendIfActive(findings, waivers, Finding{
			File: path, Line: lineNo, Rule: RuleUnlockedHeavy, Severity: SeverityWarn,
			Message: "passo docker-heavy sem admissao civm pode colidir no daemon", Remediation: remediationUnlockedHeavy,
		}, idx, unlockedHeavy)
	}
	findings = append(findings, orphanWaiverFindings(path, lines, waivers)...)
	return findings, nil
}

// fileHasProjectScope reports whether the workflow exports a project name once,
// covering compose invocations that rely on an env-level COMPOSE_PROJECT_NAME.
func fileHasProjectScope(lines []string) bool {
	for _, raw := range lines {
		if strings.Contains(raw, "COMPOSE_PROJECT_NAME") {
			return true
		}
	}
	return false
}

// protectsDockerHeavy proves that the docker-heavy command is in the payload of
// the central wrapper. Matching unrelated text elsewhere on the YAML line would
// create a false green for `heavy && wrapper` or `wrapper -- light && heavy`.
// The preferred wrapper explicitly requests a heavy cgroup slot, the global
// Docker sub-slot, and execution of that payload.
func protectsDockerHeavy(line string) bool {
	if legacyLockProtectsDockerHeavy(line) {
		return true
	}
	return admitProtectsDockerHeavy(line)
}

func legacyLockProtectsDockerHeavy(line string) bool {
	options, payload, ok := wrapperPayload(line, legacyLockWrapRe)
	if !ok || legacyLockSubcmdRe.MatchString(options) {
		return false
	}
	return execWrapRe.MatchString(options) && payloadStartsWithDockerHeavy(payload)
}

func admitProtectsDockerHeavy(line string) bool {
	options, payload, ok := wrapperPayload(line, admitWrapRe)
	if !ok {
		return false
	}
	return heavyWeightRe.MatchString(options) &&
		dockerExclusiveRe.MatchString(options) &&
		execWrapRe.MatchString(options) &&
		payloadStartsWithDockerHeavy(payload)
}

// wrapperPayload returns wrapper options and the command payload after its
// standalone `--` separator. The separator is required so flags belonging to a
// later shell command cannot be mistaken for admission flags.
func wrapperPayload(line string, wrapper *regexp.Regexp) (string, string, bool) {
	match := wrapper.FindStringIndex(line)
	if match == nil {
		return "", "", false
	}
	args := line[match[1]:]
	separator := payloadSeparatorRe.FindStringIndex(args)
	if separator == nil {
		return "", "", false
	}
	return args[:separator[0]], args[separator[1]:], true
}

// payloadStartsWithDockerHeavy accepts a heavy command only before the first
// shell control operator. That accepts `wrapper -- ci up | tee` (the heavy
// child is wrapped) and rejects `wrapper -- test && ci up` (the latter runs
// outside the wrapper).
func payloadStartsWithDockerHeavy(payload string) bool {
	heavy := dockerHeavyUpRe.FindStringIndex(payload)
	if heavy == nil {
		return false
	}
	control := shellControlRe.FindStringIndex(payload)
	return control == nil || heavy[0] < control[0]
}

// appendIfActive appends finding when matched is true and the line is not
// suppressed by a waiver targeting this rule; it also marks the waiver matched.
func appendIfActive(dst []Finding, waivers map[int]*waiver, finding Finding, idx int, matched bool) []Finding {
	if !matched {
		return dst
	}
	if w, ok := waivers[idx]; ok && w.rule == finding.Rule {
		w.matched = true
		return dst
	}
	return append(dst, finding)
}

// orphanWaiverFindings emits a warn for every waiver whose target rule was
// never matched (a stale allow comment).
func orphanWaiverFindings(path string, lines []string, waivers map[int]*waiver) []Finding {
	var findings []Finding
	for target, w := range waivers {
		if w.matched {
			continue
		}
		line := 0
		if target >= 0 && target < len(lines) {
			line = target + 1
		}
		findings = append(findings, Finding{
			File: path, Line: line, Rule: RuleOrphanWaiver, Severity: SeverityWarn,
			Message:     fmt.Sprintf("waiver para %q nao casou nenhum finding", w.rule),
			Remediation: remediationOrphanWaiver,
		})
	}
	return findings
}

// parseWaiver extracts a "# civm:ci-guard-allow <rule> <motivo>" directive.
func parseWaiver(line string) *waiver {
	hash := strings.Index(line, "#")
	if hash < 0 {
		return nil
	}
	comment := strings.TrimSpace(line[hash+1:])
	if !strings.HasPrefix(comment, waiverPrefix) {
		return nil
	}
	fields := strings.Fields(comment)
	if len(fields) < 2 {
		return nil
	}
	return &waiver{rule: fields[1]}
}

// nextSignificantLine returns the index of the next non-empty, non-comment line
// after idx, or -1 when none exists.
func nextSignificantLine(lines []string, idx int) int {
	for i := idx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return i
	}
	return -1
}

func readLines(opts Options, path string) ([]string, error) {
	data, err := opts.ReadFileFn(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return strings.Split(string(data), "\n"), nil
}

// RenderJSON emits the machine-readable result.
func RenderJSON(w io.Writer, r Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// RenderText writes a human-readable report.
func RenderText(w io.Writer, r Result) {
	fmt.Fprintf(w, "ci-guard: mode=%s findings=%d violations=%d\n", r.Mode, len(r.Findings), r.Violations)
	if len(r.Findings) == 0 {
		fmt.Fprintln(w, "OK: nenhum finding.")
		return
	}
	fmt.Fprintf(w, "%-9s %-26s %s\n", "SEVERITY", "RULE", "LOCATION")
	fmt.Fprintln(w, strings.Repeat("-", 72))
	for _, f := range r.Findings {
		fmt.Fprintf(w, "%-9s %-26s %s:%d\n", f.Severity, f.Rule, f.File, f.Line)
		fmt.Fprintf(w, "    %s\n", f.Message)
		fmt.Fprintf(w, "    fix: %s\n", f.Remediation)
	}
}
