package ciguard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	composeName  = "docker-compose.yml"
	workflowName = "ci.yml"
)

// writeRepoFile writes a fixture under repoRoot and creates parent dirs.
func writeRepoFile(t *testing.T, repoRoot string, rel string, content string) {
	t.Helper()
	path := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// writeCompose writes an infra compose fixture.
func writeCompose(t *testing.T, repoRoot, content string) {
	t.Helper()
	writeRepoFile(t, repoRoot, filepath.Join("infra", composeName), content)
}

// writeWorkflow writes a .github/workflows fixture.
func writeWorkflow(t *testing.T, repoRoot, content string) {
	t.Helper()
	writeRepoFile(t, repoRoot, filepath.Join(".github", "workflows", workflowName), content)
}

func findingForRule(findings []Finding, rule string) (Finding, bool) {
	for _, f := range findings {
		if f.Rule == rule {
			return f, true
		}
	}
	return Finding{}, false
}

func TestScanRuleR1ContainerName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCompose(t, root, "services:\n  db:\n    image: postgres:18\n    container_name: acme-db\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	f, ok := findingForRule(result.Findings, RuleContainerName)
	if !ok {
		t.Fatalf("expected R1 finding, got %+v", result.Findings)
	}
	if f.Severity != SeverityError {
		t.Fatalf("R1 severity = %q, want error", f.Severity)
	}
	if f.Line != 4 {
		t.Fatalf("R1 line = %d, want 4", f.Line)
	}
	if result.Violations != 1 {
		t.Fatalf("violations = %d, want 1", result.Violations)
	}
}

func TestScanRuleR2StaticHostPort(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCompose(t, root, "services:\n  web:\n    ports:\n      - \"8080:80\"\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	f, ok := findingForRule(result.Findings, RuleStaticHostPort)
	if !ok {
		t.Fatalf("expected R2 finding, got %+v", result.Findings)
	}
	if f.Severity != SeverityError {
		t.Fatalf("R2 severity = %q, want error", f.Severity)
	}
	if result.Violations != 1 {
		t.Fatalf("violations = %d, want 1", result.Violations)
	}
}

func TestScanRuleR2IgnoresEnvInterpolatedPort(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCompose(t, root, "services:\n  web:\n    ports:\n      - \"${CIVM_PORT_BASE}:80\"\n      - \"80\"\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	if _, ok := findingForRule(result.Findings, RuleStaticHostPort); ok {
		t.Fatalf("env-interpolated/single port must not trigger R2, got %+v", result.Findings)
	}
}

func TestScanRuleR3MissingProjectName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "jobs:\n  build:\n    steps:\n      - run: docker compose up -d\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	f, ok := findingForRule(result.Findings, RuleMissingProject)
	if !ok {
		t.Fatalf("expected R3 finding, got %+v", result.Findings)
	}
	if f.Severity != SeverityError {
		t.Fatalf("R3 severity = %q, want error", f.Severity)
	}
}

func TestScanRuleR3SatisfiedByProjectName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "jobs:\n  build:\n    steps:\n      - run: docker compose -p civm up -d\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	if _, ok := findingForRule(result.Findings, RuleMissingProject); ok {
		t.Fatalf("project name should suppress R3, got %+v", result.Findings)
	}
}

func TestScanRuleR4UnlockedDockerHeavyIsWarnOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// COMPOSE_PROJECT_NAME present so R3 does not fire; bare "make up" with no lock.
	writeWorkflow(t, root, "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: make up-local\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	f, ok := findingForRule(result.Findings, RuleUnlockedHeavy)
	if !ok {
		t.Fatalf("expected R4 finding, got %+v", result.Findings)
	}
	if f.Severity != SeverityWarn {
		t.Fatalf("R4 severity = %q, want warn", f.Severity)
	}
	if result.Violations != 0 {
		t.Fatalf("R4 must not count as a violation, got %d", result.Violations)
	}
}

func TestScanRuleR4SuppressedByLockWrapper(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl lock --exec -- make up-local\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	if _, ok := findingForRule(result.Findings, RuleUnlockedHeavy); ok {
		t.Fatalf("lock wrapper should suppress R4, got %+v", result.Findings)
	}
}

func TestScanRuleR4BareFlockDoesNotSuppress(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Regression for the 2026-06-14 incident: a repo-local flock is NOT
	// cross-repo protection — only `civmctl lock` makes the disk-watchdog
	// cleanup defer. Before this hardening the step passed R4 silently and a
	// concurrent prune tick swept an in-flight containerd overlayfs snapshot,
	// corrupting the image extract. A bare flock must now still trip R4.
	writeWorkflow(t, root, "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: flock \"$CI_LOCAL_LOCK\" -- make up-local\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	f, ok := findingForRule(result.Findings, RuleUnlockedHeavy)
	if !ok {
		t.Fatalf("bare repo-local flock must NOT suppress R4, got %+v", result.Findings)
	}
	if f.Severity != SeverityWarn {
		t.Fatalf("R4 severity = %q, want warn", f.Severity)
	}
}

func TestScanRuleR4DevctlCiUpIsHeavy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// O e2e-tenant-isolation.yml sobe o stack via `devctl ci up core` (que por
	// dentro roda make up-local-smoke) — docker-heavy real. Sem civmctl lock o R4
	// deve acusar; antes do ADR-107 esse padrao era invisivel ao guard.
	writeWorkflow(t, root, "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: go run ./tools/devctl/cmd/devctl ci up core\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	f, ok := findingForRule(result.Findings, RuleUnlockedHeavy)
	if !ok {
		t.Fatalf("devctl ci up deve disparar R4, got %+v", result.Findings)
	}
	if f.Severity != SeverityWarn {
		t.Fatalf("R4 severity = %q, want warn", f.Severity)
	}
}

func TestScanRuleR4MakeUpdateIsNotHeavy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// `make update`/`make upstream` NAO sao docker-heavy: o `\b` apos `up` em
	// dockerHeavyUpRe impede o falso-positivo (regressao do branch make, que
	// antes casava qualquer token comecando com "up").
	writeWorkflow(t, root, "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  m:\n    steps:\n      - run: make update-schemas\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	if _, ok := findingForRule(result.Findings, RuleUnlockedHeavy); ok {
		t.Fatalf("make update NAO deve disparar R4, got %+v", result.Findings)
	}
}

func TestScanRuleR4DevctlCiUpSuppressedByLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Par positivo do teste acima: com civmctl lock envolvendo o `devctl ci up`,
	// o R4 nao deve disparar (a protecao legitima passa).
	writeWorkflow(t, root, "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl lock --exec -- go run ./tools/devctl/cmd/devctl ci up core\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	if _, ok := findingForRule(result.Findings, RuleUnlockedHeavy); ok {
		t.Fatalf("civmctl lock deve suprimir R4 para devctl ci up, got %+v", result.Findings)
	}
}

func TestScanRuleR4AdmitDockerWrapperProtectsOnlyTheWrappedHeavyStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		workflow    string
		wantFinding bool
	}{
		{
			name:     "accepts explicit heavy docker admission around devctl up",
			workflow: "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl admit --weight heavy --exclusive docker --wait-minutes 75 --exec -- go run ./tools/devctl/cmd/devctl ci up core\n",
		},
		{
			name:     "accepts equals form for explicit heavy docker admission",
			workflow: "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl admit --weight=heavy --exclusive=docker --exec -- go run ./tools/devctl/cmd/devctl ci up core\n",
		},
		{
			name:        "rejects admission without docker exclusivity",
			workflow:    "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl admit --weight heavy --exec -- go run ./tools/devctl/cmd/devctl ci up core\n",
			wantFinding: true,
		},
		{
			name:        "rejects admission without explicit heavy weight",
			workflow:    "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl admit --exclusive docker --exec -- go run ./tools/devctl/cmd/devctl ci up core\n",
			wantFinding: true,
		},
		{
			name:        "does not let a separate legacy lock protect another step",
			workflow:    "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl lock --exec --scope docker-heavy -- make test\n      - run: go run ./tools/devctl/cmd/devctl ci up core\n",
			wantFinding: true,
		},
		{
			name:        "does not let a later legacy lock protect an earlier heavy command",
			workflow:    "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: go run ./tools/devctl/cmd/devctl ci up core && civmctl lock --exec -- make test\n",
			wantFinding: true,
		},
		{
			name:        "does not let admission protect a heavy command after its payload",
			workflow:    "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl admit --weight heavy --exclusive docker --exec -- make test && go run ./tools/devctl/cmd/devctl ci up core\n",
			wantFinding: true,
		},
		{
			name:        "does not accept a nonexecuting legacy acquire subcommand",
			workflow:    "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl lock acquire --scope docker-heavy -- go run ./tools/devctl/cmd/devctl ci up core\n",
			wantFinding: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeWorkflow(t, root, tt.workflow)

			result, err := Scan(DefaultOptions(root))
			if err != nil {
				t.Fatalf("Scan err = %v", err)
			}
			_, gotFinding := findingForRule(result.Findings, RuleUnlockedHeavy)
			if gotFinding != tt.wantFinding {
				t.Fatalf("R4 finding = %v, want %v; findings=%+v", gotFinding, tt.wantFinding, result.Findings)
			}
		})
	}
}

func TestScanWaiverSuppressesFinding(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCompose(t, root, "services:\n  db:\n    # civm:ci-guard-allow R1-container-name legado documentado\n    container_name: acme-db\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	if _, ok := findingForRule(result.Findings, RuleContainerName); ok {
		t.Fatalf("waiver should suppress R1, got %+v", result.Findings)
	}
	if _, ok := findingForRule(result.Findings, RuleOrphanWaiver); ok {
		t.Fatalf("matched waiver must not be orphan, got %+v", result.Findings)
	}
	if result.Violations != 0 {
		t.Fatalf("violations = %d, want 0 after waiver", result.Violations)
	}
}

func TestScanOrphanWaiverWarns(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Waiver targets a clean line; no R1 finding exists -> orphan.
	writeCompose(t, root, "services:\n  db:\n    # civm:ci-guard-allow R1-container-name sem motivo real\n    image: postgres:18\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	f, ok := findingForRule(result.Findings, RuleOrphanWaiver)
	if !ok {
		t.Fatalf("expected orphan-waiver finding, got %+v", result.Findings)
	}
	if f.Severity != SeverityWarn {
		t.Fatalf("orphan-waiver severity = %q, want warn", f.Severity)
	}
	if result.Violations != 0 {
		t.Fatalf("orphan-waiver must not be a violation, got %d", result.Violations)
	}
}

func TestScanConformingRepoHasNoFindings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCompose(t, root, "services:\n  web:\n    image: nginx\n    ports:\n      - \"${CIVM_PORT_BASE}:80\"\n")
	writeWorkflow(t, root, "env:\n  COMPOSE_PROJECT_NAME: civm\njobs:\n  e2e:\n    steps:\n      - run: civmctl lock --exec -- docker compose -p civm up -d\n")

	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("conforming repo should be clean, got %+v", result.Findings)
	}
	if result.Violations != 0 {
		t.Fatalf("violations = %d, want 0", result.Violations)
	}
}

func TestScanMissingRepoIsClean(t *testing.T) {
	t.Parallel()
	root := t.TempDir() // empty: no infra/, no .github/
	result, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan err = %v", err)
	}
	if len(result.Findings) != 0 || result.Violations != 0 {
		t.Fatalf("empty repo should be clean, got %+v", result)
	}
}

func TestScanReadFileErrorIsInjectable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCompose(t, root, "services:\n  db:\n    container_name: x\n")
	opts := DefaultOptions(root)
	opts.ReadFileFn = func(string) ([]byte, error) {
		return nil, os.ErrPermission
	}
	if _, err := Scan(opts); err == nil {
		t.Fatal("expected error when ReadFileFn fails")
	}
}

func TestRenderJSONRoundTrip(t *testing.T) {
	t.Parallel()
	result := Result{
		Mode:       ModeEnforce,
		Violations: 1,
		Findings: []Finding{{
			File: "infra/docker-compose.yml", Line: 4, Rule: RuleContainerName,
			Severity: SeverityError, Message: "m", Remediation: "r",
		}},
	}
	var buf bytes.Buffer
	if err := RenderJSON(&buf, result); err != nil {
		t.Fatalf("RenderJSON err = %v", err)
	}
	var parsed Result
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if parsed.Mode != ModeEnforce || parsed.Violations != 1 || len(parsed.Findings) != 1 {
		t.Fatalf("round-trip mismatch: %+v", parsed)
	}
	if parsed.Findings[0].Severity != SeverityError {
		t.Fatalf("severity not preserved: %+v", parsed.Findings[0])
	}
}

func TestRenderTextIncludesFindings(t *testing.T) {
	t.Parallel()
	result := Result{
		Mode:       ModeReport,
		Violations: 1,
		Findings: []Finding{{
			File: "infra/docker-compose.yml", Line: 4, Rule: RuleContainerName,
			Severity: SeverityError, Message: "container fixo", Remediation: "remova",
		}},
	}
	var buf bytes.Buffer
	RenderText(&buf, result)
	out := buf.String()
	for _, want := range []string{"ci-guard", RuleContainerName, "error", "container fixo", "fix: remova"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTextEmptyResult(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	RenderText(&buf, Result{Mode: ModeReport})
	if !strings.Contains(buf.String(), "OK") {
		t.Fatalf("expected OK message, got %q", buf.String())
	}
}

func TestParseWaiver(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line     string
		wantRule string
		wantNil  bool
	}{
		{line: "    # civm:ci-guard-allow R1-container-name motivo aqui", wantRule: RuleContainerName},
		{line: "container_name: x  # civm:ci-guard-allow R1-container-name inline", wantRule: RuleContainerName},
		{line: "# civm:ci-guard-allow R2-static-host-port", wantRule: RuleStaticHostPort},
		{line: "# civm:ci-guard-allow", wantNil: true},
		{line: "# unrelated comment", wantNil: true},
		{line: "no comment here", wantNil: true},
	}
	for _, c := range cases {
		got := parseWaiver(c.line)
		if c.wantNil {
			if got != nil {
				t.Fatalf("parseWaiver(%q) = %+v, want nil", c.line, got)
			}
			continue
		}
		if got == nil || got.rule != c.wantRule {
			t.Fatalf("parseWaiver(%q) = %+v, want rule %q", c.line, got, c.wantRule)
		}
	}
}

func TestNextSignificantLine(t *testing.T) {
	t.Parallel()
	lines := []string{"# waiver", "", "   # another comment", "container_name: x", "image: y"}
	if got := nextSignificantLine(lines, 0); got != 3 {
		t.Fatalf("nextSignificantLine = %d, want 3", got)
	}
	if got := nextSignificantLine(lines, 4); got != -1 {
		t.Fatalf("nextSignificantLine at end = %d, want -1", got)
	}
}

func TestIsComposeFile(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"docker-compose.yml":          true,
		"docker-compose.yaml":         true,
		"docker-compose.override.yml": true,
		"compose.yml":                 false,
		"docker-compose.txt":          false,
		"Dockerfile":                  false,
	}
	for name, want := range cases {
		if got := isComposeFile(name); got != want {
			t.Fatalf("isComposeFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	t.Parallel()
	opts := Options{}
	applyDefaults(&opts)
	if opts.RepoRoot != "." {
		t.Fatalf("RepoRoot = %q, want .", opts.RepoRoot)
	}
	if opts.Mode != ModeReport {
		t.Fatalf("Mode = %q, want report", opts.Mode)
	}
	if opts.GlobFn == nil || opts.ReadFileFn == nil || opts.WalkFn == nil {
		t.Fatalf("default funcs not installed: %+v", opts)
	}
}
