package metrics

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender_SingleGauge(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := Render(&buf, []Metric{
		{Name: "civm_disk_used_pct", Help: "Percentage of disk used", Type: TypeGauge, Value: 42},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "# HELP civm_disk_used_pct Percentage of disk used\n# TYPE civm_disk_used_pct gauge\ncivm_disk_used_pct 42\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestRender_LabelsSortedDeterministic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := Render(&buf, []Metric{
		{
			Name: "civm_hook_invocations_total",
			Help: "Count of hook invocations by event",
			Type: TypeCounter,
			Labels: map[string]string{
				"event":  "job-started",
				"result": "ok",
			},
			Value: 5,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `civm_hook_invocations_total{event="job-started",result="ok"} 5`) {
		t.Errorf("labels not deterministic: %s", out)
	}
}

func TestRender_MultipleMetricsGrouped(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := Render(&buf, []Metric{
		{Name: "civm_hook_invocations_total", Help: "c", Type: TypeCounter,
			Labels: map[string]string{"event": "job-started"}, Value: 3},
		{Name: "civm_hook_invocations_total", Type: TypeCounter,
			Labels: map[string]string{"event": "job-completed"}, Value: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Single HELP/TYPE header even with multiple samples
	if strings.Count(out, "# TYPE civm_hook_invocations_total") != 1 {
		t.Errorf("TYPE header repeated:\n%s", out)
	}
	for _, want := range []string{"job-started", "job-completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing label value %q:\n%s", want, out)
		}
	}
}

func TestRender_NamesSortedDeterministic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_ = Render(&buf, []Metric{
		{Name: "z_metric", Type: TypeGauge, Value: 1},
		{Name: "a_metric", Type: TypeGauge, Value: 2},
		{Name: "m_metric", Type: TypeGauge, Value: 3},
	})
	out := buf.String()
	idxA, idxM, idxZ := strings.Index(out, "a_metric"), strings.Index(out, "m_metric"), strings.Index(out, "z_metric")
	if idxA >= idxM || idxM >= idxZ {
		t.Errorf("not sorted: a=%d m=%d z=%d\n%s", idxA, idxM, idxZ, out)
	}
}

func TestRender_LabelValueEscaping(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_ = Render(&buf, []Metric{{
		Name: "civm_x", Type: TypeGauge,
		Labels: map[string]string{"path": `a"b\c` + "\nd"},
		Value:  1,
	}})
	out := buf.String()
	if !strings.Contains(out, `path="a\"b\\c\nd"`) {
		t.Errorf("escaping wrong: %s", out)
	}
}

func TestRender_FloatFormat(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_ = Render(&buf, []Metric{
		{Name: "x", Type: TypeGauge, Value: 0.5},
	})
	if !strings.Contains(buf.String(), "x 0.5\n") {
		t.Errorf("float fmt: %s", buf.String())
	}
}

func TestWriteTextfile_AtomicViaRename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "civm.prom")
	err := WriteTextfile(path, []Metric{
		{Name: "civm_test", Type: TypeGauge, Value: 1},
	})
	if err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "civm_test 1") {
		t.Errorf("file content wrong: %s", data)
	}
	// No leftover .tmp files
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteTextfile_OverwritesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "civm.prom")
	if err := os.WriteFile(path, []byte("stale content"), 0644); err != nil { //nolint:gosec // G306: textfile collector expects world-readable
		t.Fatal(err)
	}
	if err := WriteTextfile(path, []Metric{{Name: "fresh", Type: TypeGauge, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "stale") {
		t.Errorf("stale content survived: %s", data)
	}
}

func TestWriteTextfile_EmptyPathRejected(t *testing.T) {
	t.Parallel()
	err := WriteTextfile("", nil)
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestWriteTextfile_CreatesParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c", "civm.prom")
	if err := WriteTextfile(nested, []Metric{{Name: "x", Type: TypeGauge, Value: 1}}); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestWriteTextfile_MkdirFails(t *testing.T) {
	t.Parallel()
	// /proc é tipicamente read-only para mkdir; usar como parent força erro.
	err := WriteTextfile("/proc/sys/cant-create/civm.prom", nil)
	if err == nil {
		t.Fatal("expected mkdir error under /proc")
	}
}

func TestWriteTextfile_RenameRejectsCrossDevice(t *testing.T) {
	t.Parallel()
	// Directory existe mas sem write perm — falha em createTemp.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil { //nolint:gosec // G302: test exercita write-denied no t.TempDir
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) }) //nolint:gosec // G302: restaurar t.TempDir antes de cleanup
	err := WriteTextfile(filepath.Join(dir, "civm.prom"), nil)
	if err == nil {
		t.Fatal("expected error writing into read-only dir")
	}
}

// failingWriter erra após n writes para exercitar caminhos de erro em Render.
type failingWriter struct {
	n   int
	cur int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.cur++
	if f.cur > f.n {
		return 0, errFailingWriter
	}
	return len(p), nil
}

var errFailingWriter = &writerErr{}

type writerErr struct{}

func (*writerErr) Error() string { return "writer closed" }

func TestRender_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"help", "type", "value"} {
		cases := map[string]int{"help": 0, "type": 1, "value": 2}
		w := &failingWriter{n: cases[name]}
		err := Render(w, []Metric{{Name: "x", Help: "h", Type: TypeGauge, Value: 1}})
		if err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
