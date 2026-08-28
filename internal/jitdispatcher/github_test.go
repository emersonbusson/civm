package jitdispatcher

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "github_pat_abcdefghijklmnopqrstuvwxyz0123456789"

func TestDispatchWorkflowRequiresCurrent200Identity(t *testing.T) {
	inputs := validDispatchInputs()
	tests := []struct {
		name      string
		status    int
		body      func(string) string
		ambiguous bool
		httpCode  int
	}{
		{"200 success", http.StatusOK, validDispatchBody, false, 0},
		{"legacy 204", http.StatusNoContent, func(string) string { return "" }, true, 0},
		{"empty 200", http.StatusOK, func(string) string { return "" }, true, 0},
		{"duplicate ID", http.StatusOK, func(base string) string {
			return `{"workflow_run_id":42,"workflow_run_id":43,"run_url":"` + base + `/repos/acme/site/actions/runs/42","html_url":"https://github.example/acme/site/actions/runs/42"}`
		}, true, 0},
		{"missing ID", http.StatusOK, func(base string) string {
			return `{"run_url":"` + base + `/repos/acme/site/actions/runs/42","html_url":"https://github.example/acme/site/actions/runs/42"}`
		}, true, 0},
		{"duplicate identity", http.StatusOK, func(base string) string {
			return `{"workflow_run_id":42,"run_url":"` + base + `/repos/acme/site/actions/runs/42","html_url":"` + base + `/repos/acme/site/actions/runs/42"}`
		}, true, 0},
		{"server failure", http.StatusBadGateway, func(string) string { return testToken }, true, 0},
		{"validation failure", http.StatusUnprocessableEntity, func(string) string { return testToken }, false, http.StatusUnprocessableEntity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assertGitHubRequest(t, request, http.MethodPost, "/repos/acme/site/actions/workflows/civm-jit.yml/dispatches")
				body, err := io.ReadAll(request.Body)
				if err != nil || !strings.Contains(string(body), `"ref":"main"`) {
					t.Errorf("dispatch body = %q, error = %v", body, err)
				}
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body(server.URL)))
			}))
			defer server.Close()
			client := newTestClient(t, server)
			receipt, err := client.DispatchWorkflow(context.Background(), "acme/site", ".github/workflows/civm-jit.yml", "refs/heads/main", inputs)
			if test.name == "200 success" {
				if err != nil || receipt.RunID != 42 {
					t.Fatalf("DispatchWorkflow() = %+v, %v", receipt, err)
				}
				return
			}
			if err == nil {
				t.Fatal("DispatchWorkflow() accepted a refused response")
			}
			if errors.Is(err, ErrAmbiguous) != test.ambiguous {
				t.Fatalf("ambiguous=%v, error=%v", test.ambiguous, err)
			}
			if test.httpCode != 0 && !IsHTTPStatus(err, test.httpCode) {
				t.Fatalf("HTTP status error = %v", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error leaked response/token: %v", err)
			}
		})
	}
}

func TestDispatchWorkflowTreatsTransportFailureAsAmbiguous(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(testToken)
	})}
	client, err := NewGitHubClient("https://api.github.test", SupportedAPIVersion, []byte(testToken), httpClient)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DispatchWorkflow(context.Background(), "acme/site", ".github/workflows/civm-jit.yml", "refs/heads/main", validDispatchInputs())
	if !errors.Is(err, ErrAmbiguous) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("transport error = %v", err)
	}
}

func TestGitHubClientRepositoryRunJobAndJITEndpoints(t *testing.T) {
	workflow := []byte("name: trusted\n")
	targetLabel := "civm-jit-" + strings.Repeat("d", 64)
	identity, _ := NewIdentity(strings.NewReader(strings.Repeat("z", 32)))
	runnerExists := true
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/site":
			assertGitHubRequest(t, request, http.MethodGet, request.URL.Path)
			writeJSON(writer, `{"default_branch":"main"}`)
		case "/repos/acme/site/git/ref/heads/feature/safe":
			writeJSON(writer, `{"object":{"sha":"`+strings.Repeat("a", 40)+`"}}`)
		case "/repos/acme/site/contents/.github/workflows/civm-jit.yml":
			if request.URL.Query().Get("ref") != strings.Repeat("b", 40) {
				t.Errorf("workflow ref query = %q", request.URL.RawQuery)
			}
			writeJSON(writer, `{"encoding":"base64","content":"`+base64.StdEncoding.EncodeToString(workflow)+`","sha":"`+strings.Repeat("c", 40)+`"}`)
		case "/repos/acme/site/actions/runners":
			writeJSON(writer, `{"total_count":1,"runners":[{"labels":[{"name":"`+targetLabel+`"}]}]}`)
		case "/repos/acme/site/actions/runs/42":
			writeJSON(writer, `{"id":42,"event":"workflow_dispatch","head_sha":"`+strings.Repeat("b", 40)+`","display_title":"CIVM JIT nonce","status":"queued","conclusion":""}`)
		case "/repos/acme/site/actions/runs/42/jobs":
			writeJSON(writer, `{"total_count":1,"jobs":[{"id":7,"run_id":42,"name":"trusted-jit","status":"queued","conclusion":"","labels":["civm-jit-target"]}]}`)
		case "/repos/acme/site/actions/jobs/7":
			writeJSON(writer, `{"id":7,"run_id":42,"name":"trusted-jit","status":"completed","conclusion":"success","labels":["`+identity.Label+`"],"runner_id":99,"runner_name":"`+identity.RunnerName+`","runner_group_id":1}`)
		case "/repos/acme/site/actions/runners/generate-jitconfig":
			assertGitHubRequest(t, request, http.MethodPost, request.URL.Path)
			body, _ := io.ReadAll(request.Body)
			if strings.Contains(string(body), `"self-hosted"`) || !strings.Contains(string(body), `"labels":["civm-jit-`) {
				t.Errorf("unsafe JIT body = %s", body)
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{"runner":{"id":99,"name":"` + identity.RunnerName + `","status":"offline","busy":false,"labels":[{"name":"self-hosted","type":"read-only"},{"name":"` + identity.Label + `","type":"custom"}]},"encoded_jit_config":"encoded-jit-secret-value"}`))
		case "/repos/acme/site/actions/runs/42/cancel":
			assertGitHubRequest(t, request, http.MethodPost, request.URL.Path)
			writer.WriteHeader(http.StatusAccepted)
		case "/repos/acme/site/actions/runners/99":
			if request.Method == http.MethodDelete {
				runnerExists = false
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			if !runnerExists {
				http.NotFound(writer, request)
				return
			}
			writeJSON(writer, `{"id":99,"name":"`+identity.RunnerName+`","status":"offline","busy":false,"labels":[{"name":"self-hosted","type":"read-only"},{"name":"`+identity.Label+`","type":"custom"}]}`)
		default:
			t.Errorf("unexpected endpoint %s", request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server)
	ctx := context.Background()
	if branch, err := client.DefaultBranch(ctx, "acme/site"); err != nil || branch != "main" {
		t.Fatalf("DefaultBranch() = %q, %v", branch, err)
	}
	if sha, err := client.ResolveRef(ctx, "acme/site", "refs/heads/feature/safe"); err != nil || sha != strings.Repeat("a", 40) {
		t.Fatalf("ResolveRef() = %q, %v", sha, err)
	}
	if content, err := client.WorkflowContent(ctx, "acme/site", ".github/workflows/civm-jit.yml", strings.Repeat("b", 40)); err != nil || string(content) != string(workflow) {
		t.Fatalf("WorkflowContent() = %q, %v", content, err)
	}
	if exists, err := client.RunnerLabelExists(ctx, "acme/site", targetLabel); err != nil || !exists {
		t.Fatalf("RunnerLabelExists() = %v, %v", exists, err)
	}
	if run, err := client.GetRun(ctx, "acme/site", 42); err != nil || run.ID != 42 {
		t.Fatalf("GetRun() = %+v, %v", run, err)
	}
	if jobs, err := client.ListJobs(ctx, "acme/site", 42); err != nil || len(jobs) != 1 || jobs[0].ID != 7 {
		t.Fatalf("ListJobs() = %+v, %v", jobs, err)
	}
	if job, err := client.GetJob(ctx, "acme/site", 7); err != nil || job.RunnerID != 99 || job.RunID != 42 {
		t.Fatalf("GetJob() = %+v, %v", job, err)
	}
	registration, err := client.GenerateJIT(ctx, "acme/site", identity, 1)
	if err != nil || registration.Runner.ID != 99 || string(registration.Secret.Bytes()) != "encoded-jit-secret-value" {
		t.Fatalf("GenerateJIT() = %+v, %v", registration.Runner, err)
	}
	registration.Secret.Zero()
	if len(registration.Secret.Bytes()) != 0 {
		t.Fatal("JIT secret was not cleared")
	}
	if runner, found, err := client.GetRunner(ctx, "acme/site", 99); err != nil || !found || runner.Name != identity.RunnerName {
		t.Fatalf("GetRunner() = %+v, %v, %v", runner, found, err)
	}
	if err := client.DeleteRunner(ctx, "acme/site", 99); err != nil {
		t.Fatalf("DeleteRunner() error = %v", err)
	}
	if _, found, err := client.GetRunner(ctx, "acme/site", 99); err != nil || found {
		t.Fatalf("removed GetRunner() found/error = %v/%v", found, err)
	}
	if err := client.CancelRun(ctx, "acme/site", 42); err != nil {
		t.Fatalf("CancelRun() error = %v", err)
	}
}

func TestGenerateJITRedactsPartialFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-GitHub-Request-Id", "safe-request-1")
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte(`{"encoded_jit_config":"` + testToken + `"}`))
	}))
	defer server.Close()
	client := newTestClient(t, server)
	identity, _ := NewIdentity(strings.NewReader(strings.Repeat("q", 32)))
	_, err := client.GenerateJIT(context.Background(), "acme/site", identity, 1)
	if !errors.Is(err, ErrAmbiguous) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("GenerateJIT() error = %v", err)
	}
}

func TestGenerateJITRejectsUnexpectedCustomLabel(t *testing.T) {
	identity, _ := NewIdentity(strings.NewReader(strings.Repeat("n", 32)))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"runner":{"id":99,"name":"` + identity.RunnerName + `","status":"offline","busy":false,"labels":[{"name":"` + identity.Label + `","type":"custom"},{"name":"civm","type":"custom"}]},"encoded_jit_config":"encoded-jit-secret-value"}`))
	}))
	defer server.Close()
	client := newTestClient(t, server)
	_, err := client.GenerateJIT(context.Background(), "acme/site", identity, 1)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("GenerateJIT() label error = %v", err)
	}
}

func TestGitHubClientRejectsInvalidSetupAndArgumentsWithoutNetwork(t *testing.T) {
	for _, test := range []struct {
		base    string
		version string
		token   string
	}{
		{"http://example.test", SupportedAPIVersion, testToken},
		{"https://user@example.test", SupportedAPIVersion, testToken},
		{"https://api.github.test", "2022-11-28", testToken},
		{"https://api.github.test", SupportedAPIVersion, "short"},
	} {
		if _, err := NewGitHubClient(test.base, test.version, []byte(test.token), nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("NewGitHubClient(%q) error = %v", test.base, err)
		}
	}
	calls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not be called")
	})}
	client, err := NewGitHubClient("https://api.github.test", SupportedAPIVersion, []byte(testToken), httpClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.DefaultBranch(ctx, "bad"); !errors.Is(err, ErrInvalid) {
		t.Errorf("DefaultBranch() error = %v", err)
	}
	if _, err := client.GetRun(ctx, "acme/site", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("GetRun() error = %v", err)
	}
	if _, err := client.ListJobs(ctx, "acme/site", -1); !errors.Is(err, ErrInvalid) {
		t.Errorf("ListJobs() error = %v", err)
	}
	if _, err := client.GetJob(ctx, "acme/site", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("GetJob() error = %v", err)
	}
	if _, _, err := client.GetRunner(ctx, "acme/site", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("GetRunner() error = %v", err)
	}
	if err := client.DeleteRunner(ctx, "acme/site", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("DeleteRunner() error = %v", err)
	}
	if err := client.CancelRun(ctx, "acme/site", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("CancelRun() error = %v", err)
	}
	if _, err := client.DispatchWorkflow(ctx, "acme/site", ".github/workflows/civm-jit.yml", "refs/heads/main", map[string]string{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("DispatchWorkflow() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("invalid arguments made %d HTTP calls", calls)
	}
}

func TestRunnerLabelPaginationFindsOnlyExactNonce(t *testing.T) {
	target := "civm-jit-" + strings.Repeat("e", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		page := request.URL.Query().Get("page")
		if page == "1" {
			runners := make([]string, 100)
			for index := range runners {
				runners[index] = `{"labels":[{"name":"other"}]}`
			}
			writeJSON(writer, `{"total_count":101,"runners":[`+strings.Join(runners, ",")+`]}`)
			return
		}
		if page != "2" {
			t.Errorf("unexpected page %q", page)
		}
		writeJSON(writer, `{"total_count":101,"runners":[{"labels":[{"name":"`+target+`"}]}]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server)
	exists, err := client.RunnerLabelExists(context.Background(), "acme/site", target)
	if err != nil || !exists {
		t.Fatalf("RunnerLabelExists() = %v, %v", exists, err)
	}
}

func TestFindRunnerByLabelUsesExactCustomNonceAcrossPages(t *testing.T) {
	target := "civm-jit-" + strings.Repeat("f", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") == "1" {
			runners := make([]string, 100)
			for index := range runners {
				runners[index] = `{"id":1,"name":"other","labels":[{"name":"other","type":"custom"}]}`
			}
			writeJSON(writer, `{"total_count":101,"runners":[`+strings.Join(runners, ",")+`]}`)
			return
		}
		writeJSON(writer, `{"total_count":101,"runners":[{"id":99,"name":"civm-jit-exact","status":"offline","busy":false,"labels":[{"name":"self-hosted","type":"read-only"},{"name":"`+target+`","type":"custom"}]}]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server)
	runner, found, err := client.FindRunnerByLabel(context.Background(), "acme/site", target)
	if err != nil || !found || runner.ID != 99 || runner.Name != "civm-jit-exact" || !runnerHasLabel(runner, target) {
		t.Fatalf("FindRunnerByLabel() = %+v, %v, %v", runner, found, err)
	}
}

func TestFindRunnerByLabelRejectsDuplicateExactNonce(t *testing.T) {
	target := "civm-jit-" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, `{"total_count":2,"runners":[`+
			`{"id":1,"name":"one","labels":[{"name":"`+target+`","type":"custom"}]},`+
			`{"id":2,"name":"two","labels":[{"name":"`+target+`","type":"custom"}]}`+
			`]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server)
	if _, _, err := client.FindRunnerByLabel(context.Background(), "acme/site", target); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("FindRunnerByLabel() duplicate error = %v", err)
	}
}

func TestGitHubCleanupEndpointsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		operation func(*GitHubClient) error
		ambiguous bool
	}{
		{name: "runner already absent", status: http.StatusNotFound, operation: func(client *GitHubClient) error {
			return client.DeleteRunner(context.Background(), "acme/site", 99)
		}},
		{name: "runner delete server error", status: http.StatusBadGateway, ambiguous: true, operation: func(client *GitHubClient) error {
			return client.DeleteRunner(context.Background(), "acme/site", 99)
		}},
		{name: "run already terminal", status: http.StatusConflict, operation: func(client *GitHubClient) error {
			return client.CancelRun(context.Background(), "acme/site", 42)
		}},
		{name: "run cancel server error", status: http.StatusBadGateway, ambiguous: true, operation: func(client *GitHubClient) error {
			return client.CancelRun(context.Background(), "acme/site", 42)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
			}))
			defer server.Close()
			err := test.operation(newTestClient(t, server))
			if test.status == http.StatusNotFound {
				if err != nil {
					t.Fatalf("cleanup error = %v", err)
				}
				return
			}
			if err == nil || errors.Is(err, ErrAmbiguous) != test.ambiguous {
				t.Fatalf("cleanup error = %v, ambiguous=%v", err, test.ambiguous)
			}
		})
	}
}

func TestDispatchWorkflowDoesNotFollowRedirect(t *testing.T) {
	followed := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirected" {
			followed++
			writeJSON(writer, validDispatchBody(server.URL))
			return
		}
		writer.Header().Set("Location", server.URL+"/redirected")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := newTestClient(t, server)
	_, err := client.DispatchWorkflow(context.Background(), "acme/site", ".github/workflows/civm-jit.yml", "refs/heads/main", validDispatchInputs())
	if !errors.Is(err, ErrAmbiguous) || followed != 0 {
		t.Fatalf("redirect error/follow count = %v/%d", err, followed)
	}
}

func validDispatchInputs() map[string]string {
	return map[string]string{
		"candidate_sha": strings.Repeat("a", 40), "candidate_ref": "refs/heads/feature/safe",
		"dispatch_nonce": strings.Repeat("b", 64), "runner_label": "civm-jit-" + strings.Repeat("b", 64),
	}
}

func validDispatchBody(base string) string {
	return `{"workflow_run_id":42,"run_url":"` + base + `/repos/acme/site/actions/runs/42","html_url":"https://github.example/acme/site/actions/runs/42"}`
}

func newTestClient(t *testing.T, server *httptest.Server) *GitHubClient {
	t.Helper()
	client, err := NewGitHubClient(server.URL, SupportedAPIVersion, []byte(testToken), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func assertGitHubRequest(t *testing.T, request *http.Request, method, requestPath string) {
	t.Helper()
	if request.Method != method || request.URL.Path != requestPath {
		t.Errorf("request = %s %s, want %s %s", request.Method, request.URL.Path, method, requestPath)
	}
	if request.Header.Get("X-GitHub-Api-Version") != SupportedAPIVersion ||
		request.Header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("required GitHub headers are missing")
	}
}

func writeJSON(writer http.ResponseWriter, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(body))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
