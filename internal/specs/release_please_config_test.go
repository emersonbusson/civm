package specs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type releasePleaseConfig struct {
	SeparatePullRequests    bool                                  `json:"separate-pull-requests"`
	PullRequestTitlePattern string                                `json:"pull-request-title-pattern"`
	GroupPullRequestTitle   string                                `json:"group-pull-request-title-pattern"`
	IncludeComponentInTag   bool                                  `json:"include-component-in-tag"`
	IncludeVInTag           bool                                  `json:"include-v-in-tag"`
	Packages                map[string]map[string]json.RawMessage `json:"packages"`
}

func TestReleasePleaseGroupedModeIsComponentless(t *testing.T) {
	t.Parallel()

	cfg := loadReleasePleaseConfig(t)

	if cfg.SeparatePullRequests {
		t.Fatalf("separate-pull-requests = true, want false for one grouped release PR")
	}
	if cfg.IncludeComponentInTag {
		t.Fatalf("include-component-in-tag = true, want false for tags like v1.1.2")
	}
	if !cfg.IncludeVInTag {
		t.Fatalf("include-v-in-tag = false, want true for tags like v1.1.2")
	}

	root, ok := cfg.Packages["."]
	if !ok {
		t.Fatalf("packages[.] missing")
	}
	if _, ok := root["package-name"]; ok {
		t.Fatalf("packages[.].package-name must stay unset in grouped mode")
	}
}

func TestReleasePleaseTitlePatternsParseMergedGroupedPR(t *testing.T) {
	t.Parallel()

	cfg := loadReleasePleaseConfig(t)
	title := "chore: release civm v1.1.2"

	for name, pattern := range map[string]string{
		"pull-request-title-pattern":       cfg.PullRequestTitlePattern,
		"group-pull-request-title-pattern": cfg.GroupPullRequestTitle,
	} {
		component, version, ok := parseReleaseTitle(pattern, title)
		if !ok {
			t.Fatalf("%s=%q does not parse %q", name, pattern, title)
		}
		if component != "" {
			t.Fatalf("%s parsed component %q, want empty component", name, component)
		}
		if version != "1.1.2" {
			t.Fatalf("%s parsed version %q, want 1.1.2", name, version)
		}
	}
}

func TestReleaseWorkflowUsesGitHubAppTokenFirst(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", ".github", "workflows", "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	workflow := string(data)

	for _, want := range []string{
		"actions/create-github-app-token@v3",
		"id: release-app-token",
		"app-id: ${{ secrets.RELEASE_APP_ID }}",
		"private-key: ${{ secrets.RELEASE_APP_PRIVATE_KEY }}",
		"permission-contents: write",
		"permission-pull-requests: write",
		"permission-issues: write",
		"permission-metadata: read",
		"steps.release-app-token.outputs.token || secrets.RELEASE_PLEASE_TOKEN || secrets.GITHUB_TOKEN",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("release workflow missing %q", want)
		}
	}

	appStep := strings.Index(workflow, "actions/create-github-app-token@v3")
	releasePleaseStep := strings.Index(workflow, "googleapis/release-please-action@v4")
	if appStep < 0 || releasePleaseStep < 0 || appStep > releasePleaseStep {
		t.Fatalf("GitHub App token step must run before release-please")
	}
}

func loadReleasePleaseConfig(t *testing.T) releasePleaseConfig {
	t.Helper()

	path := filepath.Join("..", "..", "release-please-config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var cfg releasePleaseConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}

func parseReleaseTitle(pattern, title string) (component string, version string, ok bool) {
	matchPattern := regexp.QuoteMeta(pattern)
	matchPattern = regexp.MustCompile(`\\\$\\\{scope\\\}`).ReplaceAllString(matchPattern, `(\((?P<branch>[A-Za-z0-9_./-]+)\))?`)
	matchPattern = regexp.MustCompile(`\\\$\\\{component\\\}`).ReplaceAllString(matchPattern, ` ?(?P<component>@?[A-Za-z0-9_./-]*)?`)
	matchPattern = regexp.MustCompile(`\\\$\\\{version\\\}`).ReplaceAllString(matchPattern, `v?(?P<version>[0-9].*)`)
	matchPattern = regexp.MustCompile(`\\\$\\\{branch\\\}`).ReplaceAllString(matchPattern, `(?P<branch>[A-Za-z0-9_./-]+)?`)

	re := regexp.MustCompile("^" + matchPattern + "$")
	matches := re.FindStringSubmatch(title)
	if matches == nil {
		return "", "", false
	}

	for i, name := range re.SubexpNames() {
		switch name {
		case "component":
			component = matches[i]
		case "version":
			version = matches[i]
		}
	}
	return component, version, true
}
