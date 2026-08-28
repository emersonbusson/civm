// Package specs holds the target versions for the GitHub Actions
// ubuntu-latest runner image. Source of truth: actions/runner-images
// Ubuntu2404-Readme.md. Values are hardcoded on purpose so the binary
// is self-contained and reproducible.
package specs

import "fmt"

// ToolVersion is a single tool with one or more installable versions.
// First entry in Versions is the preferred one (used by bootstrap).
type ToolVersion struct {
	Name     string
	Versions []string
	Source   string
}

// Preferred returns the first version (the one bootstrap installs).
func (t ToolVersion) Preferred() string {
	if len(t.Versions) == 0 {
		return ""
	}
	return t.Versions[0]
}

// RunnerImageSpec is a snapshot of a GitHub Actions runner image.
type RunnerImageSpec struct {
	OSDistro     string
	OSVersion    string
	Kernel       string
	UpstreamURL  string
	ImageVersion string
	Tools        []ToolVersion
}

// Ubuntu2404 returns the spec snapshot for ubuntu-latest as of 2026-05-10.
// Update when actions/runner-images publishes a newer Ubuntu2404-Readme.md.
func Ubuntu2404() RunnerImageSpec {
	return RunnerImageSpec{
		OSDistro:     "Ubuntu",
		OSVersion:    "24.04.4 LTS",
		Kernel:       "6.17.0-1010-azure",
		ImageVersion: "20260413.86.1",
		UpstreamURL:  "https://github.com/actions/runner-images/blob/main/images/ubuntu/Ubuntu2404-Readme.md",
		Tools: []ToolVersion{
			{
				Name:     "go",
				Versions: []string{"1.26.3", "1.25.9", "1.24.13", "1.23.12", "1.22.12"},
				Source:   "go.dev/dl",
			},
			{
				Name:     "node",
				Versions: []string{"24.15.0", "24.14.1", "22.22.2", "20.20.2"},
				Source:   "lts/krypton via nvm; apt nodejs",
			},
			{
				Name:     "python",
				Versions: []string{"3.12.13", "3.10.20", "3.11.15", "3.13.13", "3.14.4"},
				Source:   "apt + pyenv (multi-version)",
			},
			{
				Name:     "docker",
				Versions: []string{"28.0.4"},
				Source:   "download.docker.com/linux/ubuntu",
			},
			{
				Name:     "docker-compose",
				Versions: []string{"2.38.2"},
				Source:   "docker-compose-plugin (apt)",
			},
			{
				Name:     "gh",
				Versions: []string{"2.89.0"},
				Source:   "cli.github.com/packages",
			},
			{
				Name:     "git",
				Versions: []string{"2.53.0"},
				Source:   "ppa:git-core/ppa",
			},
			{
				Name:     "jq",
				Versions: []string{"1.7"},
				Source:   "apt (jammy/noble)",
			},
			{
				Name:     "yq",
				Versions: []string{"4.52.5"},
				Source:   "github.com/mikefarah/yq/releases",
			},
		},
	}
}

// FindTool returns the ToolVersion entry by name, or false if not present.
func (s RunnerImageSpec) FindTool(name string) (ToolVersion, bool) {
	for _, t := range s.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return ToolVersion{}, false
}

// Render returns the spec as a human-readable table (PT-BR labels).
func (s RunnerImageSpec) Render() string {
	out := fmt.Sprintf("Imagem alvo: %s %s (kernel %s, build %s)\n",
		s.OSDistro, s.OSVersion, s.Kernel, s.ImageVersion)
	out += fmt.Sprintf("Source: %s\n\n", s.UpstreamURL)
	out += fmt.Sprintf("%-16s %-12s %s\n", "Tool", "Preferred", "Disponiveis")
	out += "----------------------------------------------------------\n"
	for _, t := range s.Tools {
		out += fmt.Sprintf("%-16s %-12s %v\n", t.Name, t.Preferred(), t.Versions)
	}
	return out
}
