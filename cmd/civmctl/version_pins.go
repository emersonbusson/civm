package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/emersonbusson/civm/internal/specs"
)

func runVersionPins(args []string) int {
	fs := flag.NewFlagSet("version-pins", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON em vez de tabela")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de version-pins:", err)
		return exitUsage
	}
	spec := specs.Ubuntu2404()
	if *jsonOut {
		return renderSpecJSON(spec, os.Stdout)
	}
	fmt.Print(spec.Render())
	return 0
}

func renderSpecJSON(s specs.RunnerImageSpec, w io.Writer) int {
	type toolOut struct {
		Name      string   `json:"name"`
		Preferred string   `json:"preferred"`
		Versions  []string `json:"versions"`
		Source    string   `json:"source"`
	}
	type specOut struct {
		OSDistro     string    `json:"os_distro"`
		OSVersion    string    `json:"os_version"`
		Kernel       string    `json:"kernel"`
		ImageVersion string    `json:"image_version"`
		UpstreamURL  string    `json:"upstream_url"`
		Tools        []toolOut `json:"tools"`
	}
	out := specOut{
		OSDistro:     s.OSDistro,
		OSVersion:    s.OSVersion,
		Kernel:       s.Kernel,
		ImageVersion: s.ImageVersion,
		UpstreamURL:  s.UpstreamURL,
		Tools:        make([]toolOut, 0, len(s.Tools)),
	}
	for _, t := range s.Tools {
		out.Tools = append(out.Tools, toolOut{
			Name:      t.Name,
			Preferred: t.Preferred(),
			Versions:  t.Versions,
			Source:    t.Source,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
		return 2
	}
	return 0
}
