package specs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var checkoutMajorPattern = regexp.MustCompile(`actions/checkout@v([0-9]+)`)
var setupGoMajorPattern = regexp.MustCompile(`actions/setup-go@v([0-9]+)`)

func TestCheckoutActionsUseNode24CompatibleMajor(t *testing.T) {
	t.Parallel()

	patterns := []string{
		filepath.Join("..", "..", ".github", "workflows", "*.yml"),
		filepath.Join("..", "..", ".github", "workflows", "*.yaml"),
		filepath.Join("..", "..", "templates", "*.yml.template"),
	}
	found := 0
	for _, pattern := range patterns {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for _, match := range checkoutMajorPattern.FindAllSubmatch(data, -1) {
				found++
				major, err := strconv.Atoi(string(match[1]))
				if err != nil {
					t.Fatalf("parse checkout major in %s: %v", path, err)
				}
				if major < 7 {
					t.Errorf("%s uses actions/checkout@v%d; require v7 or newer", path, major)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no actions/checkout references found in workflows or templates")
	}
}

func TestSetupGoActionsUseNode24CompatibleMajor(t *testing.T) {
	t.Parallel()

	patterns := []string{
		filepath.Join("..", "..", ".github", "workflows", "*.yml"),
		filepath.Join("..", "..", ".github", "workflows", "*.yaml"),
		filepath.Join("..", "..", "templates", "*.yml.template"),
	}
	found := 0
	for _, pattern := range patterns {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for _, match := range setupGoMajorPattern.FindAllSubmatch(data, -1) {
				found++
				major, err := strconv.Atoi(string(match[1]))
				if err != nil {
					t.Fatalf("parse setup-go major in %s: %v", path, err)
				}
				if major < 7 {
					t.Errorf("%s uses actions/setup-go@v%d; require v7 or newer", path, major)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no actions/setup-go references found in workflows or templates")
	}
}

func TestSelfHostedSmokeRejectsForksAndPaidCI(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", workflowPath, err)
	}
	workflow := string(data)
	jobStart := strings.Index(workflow, "\n  self-hosted-smoke:")
	jobEnd := strings.Index(workflow, "\n  ci:")
	if jobStart < 0 || jobEnd <= jobStart {
		t.Fatal("CI workflow must keep an isolated self-hosted smoke job")
	}
	job := workflow[jobStart:jobEnd]
	conditionStart := strings.Index(job, "\n    if:")
	conditionEnd := strings.Index(job, "\n    runs-on:")
	if conditionStart < 0 || conditionEnd <= conditionStart {
		t.Fatal("self-hosted smoke must keep an explicit admission condition")
	}
	got := strings.Join(strings.Fields(job[conditionStart:conditionEnd]), " ")
	want := strings.Join(strings.Fields(`if: >-
      needs.changes.outputs.full == 'true' &&
      vars.CIVM_SELF_HOSTED_SMOKE == 'true' && vars.CI_BACKEND != 'paid' &&
      (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)`), " ")
	if got != want {
		t.Fatalf("self-hosted smoke admission condition = %q, want %q", got, want)
	}
}
