// Package metrics emits prometheus textfile metrics for the
// node_exporter textfile collector. Atomic-write semantics ensure
// node_exporter never observes a half-written file.
//
// Format reference:
// https://prometheus.io/docs/instrumenting/exposition_formats/
package metrics

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Type identifies a prometheus metric type. Only gauge and counter are
// produced by civmctl; histograms/summaries would need a real client lib.
type Type string

const (
	TypeGauge   Type = "gauge"
	TypeCounter Type = "counter"
)

// Metric is one prometheus textfile metric record. Multiple samples
// with the same Name (different Labels) are emitted as separate Value
// lines under a single HELP/TYPE header.
type Metric struct {
	Name   string            // e.g. civm_disk_used_pct
	Help   string            // one-line description
	Type   Type              // gauge or counter
	Labels map[string]string // optional
	Value  float64
}

// Render writes prometheus textfile-formatted metrics to w. Metrics are
// grouped by Name so each HELP/TYPE header is emitted exactly once.
func Render(w io.Writer, metrics []Metric) error {
	groups := make(map[string][]Metric)
	var order []string
	for _, m := range metrics {
		if _, ok := groups[m.Name]; !ok {
			order = append(order, m.Name)
		}
		groups[m.Name] = append(groups[m.Name], m)
	}
	sort.Strings(order)
	for _, name := range order {
		group := groups[name]
		head := group[0]
		if head.Help != "" {
			if _, err := fmt.Fprintf(w, "# HELP %s %s\n", name, escapeHelp(head.Help)); err != nil {
				return err
			}
		}
		if head.Type != "" {
			if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", name, head.Type); err != nil {
				return err
			}
		}
		for _, m := range group {
			if _, err := fmt.Fprintf(w, "%s%s %s\n", name, renderLabels(m.Labels), formatValue(m.Value)); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteTextfile atomically writes metrics to path. node_exporter reads
// the file mid-scrape, so we write to path.tmp then rename — POSIX
// guarantees rename atomicity within the same filesystem.
func WriteTextfile(path string, metrics []Metric) error {
	if path == "" {
		return fmt.Errorf("metrics path required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // G301: textfile collector dir intencionalmente world-readable para node_exporter
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := Render(tmp, metrics); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("render: %w", err)
	}
	if err := tmp.Chmod(0644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}

func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `%s="%s"`, k, escapeLabelValue(labels[k]))
	}
	b.WriteString("}")
	return b.String()
}

// escapeLabelValue handles backslash, newline and double-quote per the
// prometheus text exposition format.
func escapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// escapeHelp handles backslash and newline for HELP comment per spec.
func escapeHelp(h string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(h)
}

func formatValue(v float64) string {
	// Prometheus text format accepts standard floats; %g gives compact
	// representation with sufficient precision for gauges/counters.
	return fmt.Sprintf("%g", v)
}
