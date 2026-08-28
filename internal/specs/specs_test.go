package specs

import (
	"strings"
	"testing"
)

func TestUbuntu2404_HasRequiredTools(t *testing.T) {
	t.Parallel()
	s := Ubuntu2404()
	required := []string{"go", "node", "python", "docker", "gh", "git", "jq"}
	for _, name := range required {
		if _, ok := s.FindTool(name); !ok {
			t.Errorf("ferramenta obrigatória ausente: %q", name)
		}
	}
}

func TestUbuntu2404_VersionsNonEmpty(t *testing.T) {
	t.Parallel()
	s := Ubuntu2404()
	for _, tv := range s.Tools {
		if len(tv.Versions) == 0 {
			t.Errorf("tool %s tem Versions vazio", tv.Name)
		}
		if tv.Preferred() == "" {
			t.Errorf("tool %s tem Preferred() vazio", tv.Name)
		}
		if tv.Source == "" {
			t.Errorf("tool %s tem Source vazio (necessario para audit)", tv.Name)
		}
	}
}

func TestUbuntu2404_ConcreteVersions(t *testing.T) {
	t.Parallel()
	s := Ubuntu2404()
	cases := map[string]string{
		"go":     "1.26.3",
		"node":   "24.15.0",
		"docker": "28.0.4",
		"gh":     "2.89.0",
	}
	for name, want := range cases {
		tv, ok := s.FindTool(name)
		if !ok {
			t.Fatalf("%s ausente", name)
			continue
		}
		if tv.Preferred() != want {
			t.Errorf("%s preferred = %q, want %q", name, tv.Preferred(), want)
		}
	}
}

func TestUbuntu2404_Metadata(t *testing.T) {
	t.Parallel()
	s := Ubuntu2404()
	if s.OSDistro != "Ubuntu" {
		t.Errorf("OSDistro = %q, want Ubuntu", s.OSDistro)
	}
	if !strings.HasPrefix(s.OSVersion, "24.04") {
		t.Errorf("OSVersion = %q, want prefixo 24.04", s.OSVersion)
	}
	if s.UpstreamURL == "" {
		t.Errorf("UpstreamURL vazio (audit nao funciona)")
	}
}

func TestFindTool_NotFound(t *testing.T) {
	t.Parallel()
	s := Ubuntu2404()
	if _, ok := s.FindTool("definitely-not-a-real-tool-xyz"); ok {
		t.Errorf("FindTool inventou ferramenta")
	}
}

func TestRender_ContainsAllTools(t *testing.T) {
	t.Parallel()
	s := Ubuntu2404()
	out := s.Render()
	for _, tv := range s.Tools {
		if !strings.Contains(out, tv.Name) {
			t.Errorf("Render() omitiu tool %s", tv.Name)
		}
	}
	if !strings.Contains(out, "Ubuntu") {
		t.Errorf("Render() omitiu OSDistro")
	}
}

func TestPreferred_EmptyVersions(t *testing.T) {
	t.Parallel()
	tv := ToolVersion{Name: "x", Versions: nil}
	if got := tv.Preferred(); got != "" {
		t.Errorf("Preferred() com Versions nil = %q, want vazio", got)
	}
}
