package specs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

var checkoutMajorPattern = regexp.MustCompile(`actions/checkout@v([0-9]+)`)

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
