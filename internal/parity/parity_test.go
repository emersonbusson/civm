package parity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/specs"
)

func TestCheckClassifiesInstalledTools(t *testing.T) {
	t.Parallel()
	runFn := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "go":
			return []byte("go version go1.26.3 linux/amd64\n"), nil
		case "node":
			return []byte("v24.15.0\n"), nil
		case "python3":
			return []byte("Python 3.12.13\n"), nil
		case "docker":
			if len(args) > 0 && args[0] == "compose" {
				return []byte("Docker Compose version v2.38.2\n"), nil
			}
			return []byte("Docker version 28.0.4, build abc\n"), nil
		case "gh":
			return []byte("gh version 2.89.0 (2026-03-26)\n"), nil
		case "git":
			return []byte("git version 2.53.0\n"), nil
		case "jq":
			return []byte("jq-1.7\n"), nil
		case "yq":
			return []byte("yq (https://github.com/mikefarah/yq/) version v4.52.5\n"), nil
		default:
			return nil, errors.New("unexpected command: " + name)
		}
	}
	report := Check(context.Background(), Options{Spec: specs.Ubuntu2404(), RunFn: runFn})
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, checks=%+v", report.Exit, report.Checks)
	}
}

func TestCheckReportsMissingAndBehindTools(t *testing.T) {
	t.Parallel()
	runFn := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "go":
			return []byte("go version go1.26.2 linux/amd64\n"), nil
		case "yq":
			return nil, errors.New("not found")
		default:
			return []byte("version 999.0.0\n"), nil
		}
	}
	report := Check(context.Background(), Options{Spec: specs.Ubuntu2404(), RunFn: runFn})
	if report.Exit == 0 {
		t.Fatalf("Exit = 0, want non-zero for missing/behind")
	}
	joined := report.RenderString()
	for _, want := range []string{"go", "behind", "yq", "missing"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("report missing %q:\n%s", want, joined)
		}
	}
}

func TestCheckAcceptsOSProvidedCompatibleToolFamilies(t *testing.T) {
	t.Parallel()
	runFn := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "python3":
			return []byte("Python 3.12.3\n"), nil
		case "git":
			return []byte("git version 2.43.0\n"), nil
		case "go":
			return []byte("go version go1.26.3 linux/amd64\n"), nil
		case "node":
			return []byte("v24.15.0\n"), nil
		case "docker":
			if len(args) > 0 && args[0] == "compose" {
				return []byte("Docker Compose version v2.38.2\n"), nil
			}
			return []byte("Docker version 28.0.4, build abc\n"), nil
		case "gh":
			return []byte("gh version 2.89.0 (2026-03-26)\n"), nil
		case "jq":
			return []byte("jq-1.7\n"), nil
		case "yq":
			return []byte("yq (https://github.com/mikefarah/yq/) version v4.52.5\n"), nil
		default:
			return nil, errors.New("unexpected command: " + name)
		}
	}
	report := Check(context.Background(), Options{Spec: specs.Ubuntu2404(), RunFn: runFn})
	if report.Exit != 0 {
		t.Fatalf("Exit = %d, checks=%+v", report.Exit, report.Checks)
	}
	for _, check := range report.Checks {
		if (check.Tool == "python" || check.Tool == "git") && check.Status != StatusCompat {
			t.Fatalf("%s status = %s, want compatible", check.Tool, check.Status)
		}
	}
}
