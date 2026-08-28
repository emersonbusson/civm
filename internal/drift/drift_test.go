package drift

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/specs"
)

const fakeUpstreamReadme = `
# Ubuntu 24.04 image

## Versions

- **OS Version:** 24.04.4 LTS
- **Image Version:** 99.99.99

### Languages

- **Go:** 1.22.12, 1.23.12, 1.24.13, 1.25.9
- **Node.js:** 20.20.2 (system), cached: 20.20.2, 22.22.2, 24.14.1
- **Python:** 3.12.13 (system), cached: 3.10.20, 3.11.15
- **Docker Client:** 28.0.4
- **Docker Compose v2:** 2.38.2
- **GitHub CLI (gh):** 2.89.0
- **Git:** 2.53.0
- **jq:** 1.7
- **yq:** 4.52.5
`

const upstreamWithBumps = `
- **Go:** 1.30.0, 1.29.5
- **Node.js:** 20.20.2
- **Python:** 3.12.13
- **Docker Client:** 30.0.0
- **Docker Compose v2:** 2.38.2
- **GitHub CLI (gh):** 2.89.0
- **Git:** 2.53.0
- **jq:** 1.7
- **yq:** 4.52.5
`

func TestDetect_AllInSyncOrAhead(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Fetcher = func(context.Context, string) (string, error) {
		return fakeUpstreamReadme, nil
	}
	r, err := Detect(context.Background(), opts)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.HasBehind() {
		t.Errorf("HasBehind() = true; esperava in-sync ou ahead apenas")
	}
	for _, d := range r.Diffs {
		if d.Status == StatusBehind {
			t.Errorf("%s behind: pinned=%s upstream=%s", d.Tool, d.Pinned, d.Upstream)
		}
	}
}

func TestStatusAhead(t *testing.T) {
	t.Parallel()
	if !isSemverAhead("1.26.3", []string{"1.25.9", "1.22.12"}) {
		t.Errorf("1.26.3 deveria ser ahead de [1.25.9, 1.22.12]")
	}
	if isSemverAhead("1.25.9", []string{"1.25.9", "1.26.0"}) {
		t.Errorf("1.25.9 nao deveria ser ahead se 1.26.0 esta na lista")
	}
	if isSemverAhead("", []string{"1.0.0"}) {
		t.Errorf("string vazia nao deveria ser ahead")
	}
}

func TestCompareVersion(t *testing.T) {
	t.Parallel()
	if compareVersion([]int{1, 26, 3}, []int{1, 25, 9}) != 1 {
		t.Errorf("1.26.3 vs 1.25.9 deveria ser 1")
	}
	if compareVersion([]int{1, 25, 9}, []int{1, 26, 3}) != -1 {
		t.Errorf("1.25.9 vs 1.26.3 deveria ser -1")
	}
	if compareVersion([]int{1, 26, 3}, []int{1, 26, 3}) != 0 {
		t.Errorf("igual deveria ser 0")
	}
	if compareVersion([]int{1, 26}, []int{1, 26, 0}) != 0 {
		t.Errorf("1.26 vs 1.26.0 deveria ser 0")
	}
}

func TestSplitVersion(t *testing.T) {
	t.Parallel()
	got := splitVersion("1.26.3")
	if len(got) != 3 || got[0] != 1 || got[1] != 26 || got[2] != 3 {
		t.Errorf("splitVersion(1.26.3) = %v", got)
	}
	got = splitVersion("1.7.1-3ubuntu0.24.04.1") // jq apt format
	if len(got) == 0 || got[0] != 1 {
		t.Errorf("splitVersion com sufixo = %v", got)
	}
}

func TestRender_AheadShowsInfo(t *testing.T) {
	t.Parallel()
	r := Report{
		Generated: time.Now(),
		Diffs: []Diff{
			{Tool: "go", Pinned: "1.26.3", Upstream: "1.25.9", Status: StatusAhead},
		},
	}
	var buf bytes.Buffer
	r.Render(&buf)
	if !strings.Contains(buf.String(), "INFO:") {
		t.Errorf("Ahead deveria mostrar INFO, output:\n%s", buf.String())
	}
}

func TestDetect_DetectsBehind(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Fetcher = func(context.Context, string) (string, error) {
		return upstreamWithBumps, nil
	}
	r, err := Detect(context.Background(), opts)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !r.HasBehind() {
		t.Errorf("HasBehind() = false; esperava true (Go e Docker bumped)")
	}
	behind := map[string]string{}
	for _, d := range r.Diffs {
		if d.Status == StatusBehind {
			behind[d.Tool] = d.Upstream
		}
	}
	if !strings.Contains(behind["go"], "1.30.0") {
		t.Errorf("go upstream = %q, want contains 1.30.0", behind["go"])
	}
	if !strings.Contains(behind["docker"], "30.0.0") {
		t.Errorf("docker upstream = %q, want contains 30.0.0", behind["docker"])
	}
}

func TestDetect_FetchError(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Fetcher = func(context.Context, string) (string, error) {
		return "", errors.New("rede caiu")
	}
	if _, err := Detect(context.Background(), opts); err == nil {
		t.Errorf("esperava erro propagado")
	}
}

func TestDetect_PartialUpstream(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Fetcher = func(context.Context, string) (string, error) {
		return "**Go:** 1.25.9\n", nil // somente go
	}
	r, err := Detect(context.Background(), opts)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	missingCount := 0
	for _, d := range r.Diffs {
		if d.Status == StatusUpstreamMissing {
			missingCount++
		}
	}
	if missingCount == 0 {
		t.Errorf("esperava algum tool reportado como missing-upstream")
	}
}

func TestRender_Snapshot(t *testing.T) {
	t.Parallel()
	r := Report{
		UpstreamURL: "https://example.com/readme.md",
		Generated:   time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC),
		Diffs: []Diff{
			{Tool: "go", Pinned: "1.25.9", Upstream: "1.25.9", Status: StatusInSync},
			{Tool: "docker", Pinned: "28.0.4", Upstream: "30.0.0", Status: StatusBehind},
		},
	}
	var buf bytes.Buffer
	r.Render(&buf)
	out := buf.String()
	for _, want := range []string{"go", "docker", "1.25.9", "30.0.0", "in-sync", "behind", "ATENCAO"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render() omitiu %q", want)
		}
	}
}

func TestRender_AllInSync(t *testing.T) {
	t.Parallel()
	r := Report{
		Generated: time.Now(),
		Diffs: []Diff{
			{Tool: "go", Pinned: "1.25.9", Upstream: "1.25.9", Status: StatusInSync},
		},
	}
	var buf bytes.Buffer
	r.Render(&buf)
	if !strings.Contains(buf.String(), "OK: todas") {
		t.Errorf("output sem mensagem OK")
	}
}

func TestStatusString(t *testing.T) {
	t.Parallel()
	cases := map[Status]string{
		StatusInSync:          "in-sync",
		StatusBehind:          "behind",
		StatusUpstreamMissing: "missing-upstream",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%d -> %q, want %q", s, got, want)
		}
	}
	if got := Status(99).String(); got != "?" {
		t.Errorf("Status(99) -> %q, want ?", got)
	}
}

func TestParseUpstream_AllTools(t *testing.T) {
	t.Parallel()
	got := parseUpstreamVersions(fakeUpstreamReadme)
	wantContains := map[string][]string{
		"go":             {"1.22.12", "1.25.9"},  // first and last
		"node":           {"20.20.2", "24.14.1"}, // first and last
		"python":         {"3.12.13", "3.10.20"},
		"docker":         {"28.0.4"},
		"docker-compose": {"2.38.2"},
		"gh":             {"2.89.0"},
		"git":            {"2.53.0"},
		"jq":             {"1.7"},
		"yq":             {"4.52.5"},
	}
	for tool, vs := range wantContains {
		list, ok := got[tool]
		if !ok {
			t.Errorf("tool %q ausente", tool)
			continue
		}
		for _, v := range vs {
			if !contains(list, v) {
				t.Errorf("tool %q: lista %v nao contem %q", tool, list, v)
			}
		}
	}
}

func TestParseUpstream_EmptyBody(t *testing.T) {
	t.Parallel()
	got := parseUpstreamVersions("")
	if len(got) != 0 {
		t.Errorf("parsed map = %v, want empty", got)
	}
}

func TestContains(t *testing.T) {
	t.Parallel()
	if !contains([]string{"a", "b"}, "a") {
		t.Errorf("contains([a,b], a) = false")
	}
	if contains([]string{"a", "b"}, "c") {
		t.Errorf("contains([a,b], c) = true")
	}
}

func TestUniqueStrings(t *testing.T) {
	t.Parallel()
	got := uniqueStrings([]string{"a", "b", "a", "c", "b"})
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
}

func TestParseSectionVersions(t *testing.T) {
	t.Parallel()
	body := `
#### Go

| Version |
| ------- |
| 1.22.12 |
| 1.25.9  |

#### Node.js

| Version |
| ------- |
| 20.20.2 |
| 22.22.2 |

#### Other
`
	got := parseSectionVersions(body, "go")
	if !contains(got, "1.22.12") || !contains(got, "1.25.9") {
		t.Errorf("Go section: got %v", got)
	}
	gotNode := parseSectionVersions(body, "node")
	if !contains(gotNode, "20.20.2") || !contains(gotNode, "22.22.2") {
		t.Errorf("Node section: got %v", gotNode)
	}
	// Heading not present.
	if got := parseSectionVersions(body, "rust"); len(got) != 0 {
		t.Errorf("rust section: got %v, want nil", got)
	}
}

func TestDisplayName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"go":     "Go",
		"node":   "Node.js",
		"python": "Python",
		"x":      "x",
	}
	for k, want := range cases {
		if got := displayName(k); got != want {
			t.Errorf("displayName(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestParseUpstream_RealishFormat(t *testing.T) {
	t.Parallel()
	body := `
- Node.js 20.20.2
- Python 3.12.3
- Docker Client 28.0.4
- Docker Compose v2 2.38.2
- Git 2.53.0
- jq 1.7
- yq 4.52.5
- GitHub CLI 2.89.0

#### Go

| Version |
| ------- |
| 1.22.12 |
| 1.25.9  |
`
	got := parseUpstreamVersions(body)
	cases := map[string]string{
		"node":           "20.20.2",
		"python":         "3.12.3",
		"docker":         "28.0.4",
		"docker-compose": "2.38.2",
		"git":            "2.53.0",
		"jq":             "1.7",
		"yq":             "4.52.5",
		"gh":             "2.89.0",
	}
	for tool, want := range cases {
		if !contains(got[tool], want) {
			t.Errorf("tool %q: lista %v sem %q", tool, got[tool], want)
		}
	}
	if !contains(got["go"], "1.25.9") {
		t.Errorf("go sem 1.25.9: %v", got["go"])
	}
}

func TestToRawURL(t *testing.T) {
	t.Parallel()
	in := "https://github.com/actions/runner-images/blob/main/images/ubuntu/Ubuntu2404-Readme.md"
	want := "https://raw.githubusercontent.com/actions/runner-images/main/images/ubuntu/Ubuntu2404-Readme.md"
	if got := toRawURL(in); got != want {
		t.Errorf("toRawURL = %q, want %q", got, want)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("got %q", got)
	}
	if got := truncate("abcdef", 3); !strings.HasPrefix(got, "abc") {
		t.Errorf("got %q, want prefix abc", got)
	}
	if got := truncate("abcdef", 3); !strings.Contains(got, "truncated") {
		t.Errorf("got %q, sem 'truncated'", got)
	}
}

func TestNormalizeName(t *testing.T) {
	t.Parallel()
	if normalizeName(" Go ") != "go" {
		t.Errorf("normalize falhou")
	}
}

func TestDetect_UsesSpecFromOpts(t *testing.T) {
	t.Parallel()
	opts := Options{
		Spec:        specs.Ubuntu2404(),
		UpstreamURL: "https://example.com/x.md",
		Fetcher: func(context.Context, string) (string, error) {
			return fakeUpstreamReadme, nil
		},
	}
	r, err := Detect(context.Background(), opts)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(r.Diffs) != len(opts.Spec.Tools) {
		t.Errorf("len(Diffs) = %d, want %d", len(r.Diffs), len(opts.Spec.Tools))
	}
}

// TestDetect_FetcherTimeout cobre o cenário em que o context expira durante
// a busca de upstream — common em runners com rede instável.
func TestDetect_FetcherTimeout(t *testing.T) {
	t.Parallel()
	opts := DefaultOptions()
	opts.Fetcher = func(ctx context.Context, _ string) (string, error) {
		return "", context.DeadlineExceeded
	}
	_, err := Detect(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error from timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded wrapped, got %v", err)
	}
}

// TestHTTPFetch_5xx valida que respostas não-200 são tratadas como erro
// explícito (não silenciosamente confundidas com body vazio).
// Estes testes de transporte ficam seriais: httptest.Server.Close chama
// CloseIdleConnections no http.DefaultTransport usado por httpFetch e pode
// interromper uma requisição paralela de outro servidor.
func TestHTTPFetch_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	body, err := httpFetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatalf("expected error for 5xx, got body=%q", body)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected HTTP 500 in error, got %v", err)
	}
}

// TestHTTPFetch_RateLimit429 valida tratamento de rate-limit do GitHub raw.
func TestHTTPFetch_RateLimit429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := httpFetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected HTTP 429 in error, got %v", err)
	}
}

// TestHTTPFetch_ContextCancelled valida que cancelamento de context aborta
// uma request em andamento (corner case operacional: SIGTERM / Ctrl+C).
func TestHTTPFetch_ContextCancelled(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-done // bloqueia até teste finalizar
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancela ANTES da request
	_, err := httpFetch(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// TestHTTPFetch_OK valida o caminho feliz com cabeçalhos esperados.
func TestHTTPFetch_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("User-Agent"), "civmctl-drift") {
			t.Errorf("missing User-Agent: %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()
	body, err := httpFetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body != "hello world" {
		t.Errorf("body = %q, want %q", body, "hello world")
	}
}
