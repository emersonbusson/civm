package jitdispatcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxAPIResponse = 2 << 20
	maxJITResponse = 256 << 10
)

var requestIDHeaderRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,80}$`)

type HTTPError struct {
	Operation string
	Status    int
	RequestID string
}

func (err *HTTPError) Error() string {
	if err.RequestID == "" {
		return fmt.Sprintf("GitHub API %s returned HTTP %d", err.Operation, err.Status)
	}
	return fmt.Sprintf("GitHub API %s returned HTTP %d (request %s)", err.Operation, err.Status, err.RequestID)
}

func IsHTTPStatus(err error, status int) bool {
	var target *HTTPError
	return errors.As(err, &target) && target.Status == status
}

type GitHub interface {
	DefaultBranch(context.Context, string) (string, error)
	ResolveRef(context.Context, string, string) (string, error)
	WorkflowContent(context.Context, string, string, string) ([]byte, error)
	RunnerLabelExists(context.Context, string, string) (bool, error)
	DispatchWorkflow(context.Context, string, string, string, map[string]string) (DispatchReceipt, error)
	GetRun(context.Context, string, int64) (WorkflowRun, error)
	ListJobs(context.Context, string, int64) ([]WorkflowJob, error)
	GetJob(context.Context, string, int64) (WorkflowJob, error)
	GenerateJIT(context.Context, string, Identity, int64) (JITRegistration, error)
	FindRunnerByLabel(context.Context, string, string) (RemoteRunner, bool, error)
	GetRunner(context.Context, string, int64) (RemoteRunner, bool, error)
	DeleteRunner(context.Context, string, int64) error
	CancelRun(context.Context, string, int64) error
}

type GitHubClient struct {
	base       *url.URL
	version    string
	token      []byte
	httpClient *http.Client
}

func NewGitHubClient(baseURL, version string, token []byte, client *http.Client) (*GitHubClient, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("%w: API base URL is invalid", ErrInvalid)
	}
	if base.Scheme != "https" && (base.Scheme != "http" || !isLoopbackHost(base.Hostname())) {
		return nil, fmt.Errorf("%w: API base URL must use HTTPS", ErrInvalid)
	}
	if version != SupportedAPIVersion || len(token) < 20 {
		return nil, fmt.Errorf("%w: API version or token is invalid", ErrInvalid)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copyBase := *base
	copyBase.Path = strings.TrimSuffix(copyBase.Path, "/")
	return &GitHubClient{base: &copyBase, version: version, token: token, httpClient: &clientCopy}, nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (client *GitHubClient) DefaultBranch(ctx context.Context, repository string) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	var response struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.getJSON(ctx, "get repository", client.repoPath(repository), nil, &response); err != nil {
		return "", err
	}
	ref := "refs/heads/" + response.DefaultBranch
	if err := validateRef(ref); err != nil {
		return "", fmt.Errorf("GitHub default branch is invalid")
	}
	return response.DefaultBranch, nil
}

func (client *GitHubClient) ResolveRef(ctx context.Context, repository, ref string) (string, error) {
	if err := validateRepository(repository); err != nil {
		return "", err
	}
	if err := validateRef(ref); err != nil {
		return "", err
	}
	var response struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	refPath := client.repoPath(repository) + "/git/ref/" + strings.TrimPrefix(ref, "refs/")
	if err := client.getJSON(ctx, "resolve ref", refPath, nil, &response); err != nil {
		return "", err
	}
	if !shaRE.MatchString(response.Object.SHA) {
		return "", fmt.Errorf("GitHub ref response contains an invalid SHA")
	}
	return response.Object.SHA, nil
}

func (client *GitHubClient) WorkflowContent(ctx context.Context, repository, workflow, sha string) ([]byte, error) {
	if validateRepository(repository) != nil || !workflowRE.MatchString(workflow) || !shaRE.MatchString(sha) {
		return nil, fmt.Errorf("%w: workflow or SHA is invalid", ErrInvalid)
	}
	var response struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		SHA      string `json:"sha"`
	}
	query := url.Values{"ref": []string{sha}}
	endpoint := client.repoPath(repository) + "/contents/" + workflow
	if err := client.getJSON(ctx, "get workflow", endpoint, query, &response); err != nil {
		return nil, err
	}
	if response.Encoding != "base64" || !shaRE.MatchString(response.SHA) {
		return nil, fmt.Errorf("GitHub workflow response is invalid")
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(response.Content, "\n", ""))
	if err != nil || len(content) == 0 || len(content) > maxAPIResponse {
		return nil, fmt.Errorf("GitHub workflow content is invalid")
	}
	return content, nil
}

func (client *GitHubClient) RunnerLabelExists(ctx context.Context, repository, label string) (bool, error) {
	if validateRepository(repository) != nil || !strings.HasPrefix(label, "civm-jit-") || len(label) != 73 {
		return false, fmt.Errorf("%w: repository or runner label is invalid", ErrInvalid)
	}
	for page := 1; page <= 100; page++ {
		var response struct {
			TotalCount int `json:"total_count"`
			Runners    []struct {
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
			} `json:"runners"`
		}
		query := url.Values{"per_page": []string{"100"}, "page": []string{strconv.Itoa(page)}}
		endpoint := client.repoPath(repository) + "/actions/runners"
		if err := client.getJSON(ctx, "list runners", endpoint, query, &response); err != nil {
			return false, err
		}
		for _, runner := range response.Runners {
			for _, runnerLabel := range runner.Labels {
				if runnerLabel.Name == label {
					return true, nil
				}
			}
		}
		if len(response.Runners) < 100 || page*100 >= response.TotalCount {
			return false, nil
		}
	}
	return false, fmt.Errorf("runner pagination exceeded its safety limit")
}

func (client *GitHubClient) DispatchWorkflow(
	ctx context.Context,
	repository, workflow, trustedRef string,
	inputs map[string]string,
) (DispatchReceipt, error) {
	if validateRepository(repository) != nil || !workflowRE.MatchString(workflow) ||
		validateRef(trustedRef) != nil || validateDispatchInputs(inputs) != nil {
		return DispatchReceipt{}, fmt.Errorf("%w: dispatch arguments are invalid", ErrInvalid)
	}
	body, err := json.Marshal(struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs"`
	}{Ref: strings.TrimPrefix(trustedRef, "refs/heads/"), Inputs: inputs})
	if err != nil {
		return DispatchReceipt{}, fmt.Errorf("dispatch encode: %w", err)
	}
	endpoint := client.repoPath(repository) + "/actions/workflows/" + path.Base(workflow) + "/dispatches"
	status, responseBody, requestID, err := client.do(ctx, http.MethodPost, endpoint, nil, body, maxAPIResponse)
	if err != nil {
		return DispatchReceipt{}, ambiguousTransportError(ctx, "dispatch workflow")
	}
	defer Zero(responseBody)
	if status != http.StatusOK {
		remote := &HTTPError{Operation: "dispatch workflow", Status: status, RequestID: requestID}
		if status >= 400 && status < 500 && status != http.StatusNoContent {
			return DispatchReceipt{}, remote
		}
		return DispatchReceipt{}, fmt.Errorf("%w: %w", ErrAmbiguous, remote)
	}
	var response struct {
		RunID   int64  `json:"workflow_run_id"`
		RunURL  string `json:"run_url"`
		HTMLURL string `json:"html_url"`
	}
	if err := decodeUniqueJSON(responseBody, &response); err != nil {
		return DispatchReceipt{}, fmt.Errorf("%w: dispatch response is malformed", ErrAmbiguous)
	}
	receipt := DispatchReceipt{RunID: response.RunID, RunURL: response.RunURL, HTMLURL: response.HTMLURL}
	if err := client.validateReceipt(repository, receipt); err != nil {
		return DispatchReceipt{}, fmt.Errorf("%w: dispatch response identity is invalid", ErrAmbiguous)
	}
	return receipt, nil
}

func (client *GitHubClient) validateReceipt(repository string, receipt DispatchReceipt) error {
	if receipt.RunID <= 0 || receipt.RunURL == "" || receipt.HTMLURL == "" || receipt.RunURL == receipt.HTMLURL {
		return ErrInvalid
	}
	expected := client.endpoint(client.repoPath(repository)+"/actions/runs/"+strconv.FormatInt(receipt.RunID, 10), nil)
	if receipt.RunURL != expected {
		return ErrInvalid
	}
	htmlURL, err := url.Parse(receipt.HTMLURL)
	if err != nil || htmlURL.Scheme != "https" || htmlURL.Host == "" || htmlURL.User != nil || htmlURL.RawQuery != "" || htmlURL.Fragment != "" {
		return ErrInvalid
	}
	expectedHTMLPath := "/" + repository + "/actions/runs/" + strconv.FormatInt(receipt.RunID, 10)
	if htmlURL.Path != expectedHTMLPath {
		return ErrInvalid
	}
	return nil
}

func (client *GitHubClient) GetRun(ctx context.Context, repository string, runID int64) (WorkflowRun, error) {
	if validateRepository(repository) != nil || runID <= 0 {
		return WorkflowRun{}, fmt.Errorf("%w: run ID is invalid", ErrInvalid)
	}
	var response struct {
		ID           int64  `json:"id"`
		Event        string `json:"event"`
		HeadSHA      string `json:"head_sha"`
		DisplayTitle string `json:"display_title"`
		Path         string `json:"path"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
	}
	endpoint := client.repoPath(repository) + "/actions/runs/" + strconv.FormatInt(runID, 10)
	if err := client.getJSON(ctx, "get run", endpoint, nil, &response); err != nil {
		return WorkflowRun{}, err
	}
	if response.ID != runID || !shaRE.MatchString(response.HeadSHA) {
		return WorkflowRun{}, fmt.Errorf("GitHub run identity is invalid")
	}
	return WorkflowRun(response), nil
}

func (client *GitHubClient) ListJobs(ctx context.Context, repository string, runID int64) ([]WorkflowJob, error) {
	if validateRepository(repository) != nil || runID <= 0 {
		return nil, fmt.Errorf("%w: run ID is invalid", ErrInvalid)
	}
	var jobs []WorkflowJob
	for page := 1; page <= 100; page++ {
		var response struct {
			TotalCount int `json:"total_count"`
			Jobs       []struct {
				ID            int64    `json:"id"`
				Name          string   `json:"name"`
				Status        string   `json:"status"`
				Conclusion    string   `json:"conclusion"`
				Labels        []string `json:"labels"`
				RunID         int64    `json:"run_id"`
				RunnerID      int64    `json:"runner_id"`
				RunnerName    string   `json:"runner_name"`
				RunnerGroupID int64    `json:"runner_group_id"`
			} `json:"jobs"`
		}
		query := url.Values{"per_page": []string{"100"}, "page": []string{strconv.Itoa(page)}}
		endpoint := client.repoPath(repository) + "/actions/runs/" + strconv.FormatInt(runID, 10) + "/jobs"
		if err := client.getJSON(ctx, "list jobs", endpoint, query, &response); err != nil {
			return nil, err
		}
		for _, job := range response.Jobs {
			jobs = append(jobs, WorkflowJob{
				ID: job.ID, RunID: job.RunID, Name: job.Name, Status: job.Status,
				Conclusion: job.Conclusion, Labels: append([]string(nil), job.Labels...),
				RunnerID: job.RunnerID, RunnerName: job.RunnerName, RunnerGroupID: job.RunnerGroupID,
			})
		}
		if len(response.Jobs) < 100 || len(jobs) >= response.TotalCount {
			return jobs, nil
		}
	}
	return nil, fmt.Errorf("job pagination exceeded its safety limit")
}

func (client *GitHubClient) GetJob(ctx context.Context, repository string, jobID int64) (WorkflowJob, error) {
	if validateRepository(repository) != nil || jobID <= 0 {
		return WorkflowJob{}, fmt.Errorf("%w: job ID is invalid", ErrInvalid)
	}
	var response struct {
		ID            int64    `json:"id"`
		RunID         int64    `json:"run_id"`
		Name          string   `json:"name"`
		Status        string   `json:"status"`
		Conclusion    string   `json:"conclusion"`
		Labels        []string `json:"labels"`
		RunnerID      int64    `json:"runner_id"`
		RunnerName    string   `json:"runner_name"`
		RunnerGroupID int64    `json:"runner_group_id"`
	}
	endpoint := client.repoPath(repository) + "/actions/jobs/" + strconv.FormatInt(jobID, 10)
	if err := client.getJSON(ctx, "get job", endpoint, nil, &response); err != nil {
		return WorkflowJob{}, err
	}
	if response.ID != jobID || response.RunID <= 0 {
		return WorkflowJob{}, fmt.Errorf("GitHub job identity is invalid")
	}
	return WorkflowJob(response), nil
}

func (client *GitHubClient) FindRunnerByLabel(
	ctx context.Context,
	repository, label string,
) (RemoteRunner, bool, error) {
	if validateRepository(repository) != nil || !strings.HasPrefix(label, "civm-jit-") || len(label) != 73 {
		return RemoteRunner{}, false, fmt.Errorf("%w: repository or runner label is invalid", ErrInvalid)
	}
	var match RemoteRunner
	found := false
	for page := 1; page <= 100; page++ {
		var response struct {
			TotalCount int            `json:"total_count"`
			Runners    []remoteRunner `json:"runners"`
		}
		query := url.Values{"per_page": []string{"100"}, "page": []string{strconv.Itoa(page)}}
		endpoint := client.repoPath(repository) + "/actions/runners"
		if err := client.getJSON(ctx, "list runners", endpoint, query, &response); err != nil {
			return RemoteRunner{}, false, err
		}
		for _, runner := range response.Runners {
			for _, runnerLabel := range runner.Labels {
				if runnerLabel.Name != label {
					continue
				}
				if found {
					return RemoteRunner{}, false, fmt.Errorf("%w: duplicate exact runner label", ErrAmbiguous)
				}
				match = runner.toRemote()
				found = true
			}
		}
		if len(response.Runners) < 100 || page*100 >= response.TotalCount {
			return match, found, nil
		}
	}
	return RemoteRunner{}, false, fmt.Errorf("runner pagination exceeded its safety limit")
}

func (client *GitHubClient) GetRunner(
	ctx context.Context,
	repository string,
	runnerID int64,
) (RemoteRunner, bool, error) {
	if validateRepository(repository) != nil || runnerID <= 0 {
		return RemoteRunner{}, false, fmt.Errorf("%w: runner ID is invalid", ErrInvalid)
	}
	var response remoteRunner
	endpoint := client.repoPath(repository) + "/actions/runners/" + strconv.FormatInt(runnerID, 10)
	err := client.getJSON(ctx, "get runner", endpoint, nil, &response)
	if IsHTTPStatus(err, http.StatusNotFound) {
		return RemoteRunner{}, false, nil
	}
	if err != nil {
		return RemoteRunner{}, false, err
	}
	if response.ID != runnerID {
		return RemoteRunner{}, false, fmt.Errorf("GitHub runner identity is invalid")
	}
	return response.toRemote(), true, nil
}

func (client *GitHubClient) DeleteRunner(ctx context.Context, repository string, runnerID int64) error {
	if validateRepository(repository) != nil || runnerID <= 0 {
		return fmt.Errorf("%w: runner ID is invalid", ErrInvalid)
	}
	endpoint := client.repoPath(repository) + "/actions/runners/" + strconv.FormatInt(runnerID, 10)
	status, _, requestID, err := client.do(ctx, http.MethodDelete, endpoint, nil, nil, maxAPIResponse)
	if err != nil {
		return ambiguousTransportError(ctx, "delete runner")
	}
	if status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrAmbiguous, &HTTPError{Operation: "delete runner", Status: status, RequestID: requestID})
}

type remoteRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
	Labels []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"labels"`
}

func (runner remoteRunner) toRemote() RemoteRunner {
	labels := make([]RunnerLabel, 0, len(runner.Labels))
	for _, label := range runner.Labels {
		labels = append(labels, RunnerLabel{Name: label.Name, Type: label.Type})
	}
	return RemoteRunner{ID: runner.ID, Name: runner.Name, Status: runner.Status, Busy: runner.Busy, Labels: labels}
}

type JITSecret struct {
	value []byte
}

func (secret *JITSecret) Bytes() []byte {
	if secret == nil {
		return nil
	}
	return secret.value
}

func (secret *JITSecret) Zero() {
	if secret != nil {
		Zero(secret.value)
		secret.value = nil
	}
}

type secretBytes []byte

func (value *secretBytes) UnmarshalJSON(data []byte) error {
	var decoded string
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = append((*value)[:0], decoded...)
	return nil
}

func (client *GitHubClient) GenerateJIT(
	ctx context.Context,
	repository string,
	identity Identity,
	runnerGroupID int64,
) (JITRegistration, error) {
	if validateRepository(repository) != nil || !validIdentity(identity) || runnerGroupID <= 0 {
		return JITRegistration{}, fmt.Errorf("%w: JIT request identity is invalid", ErrInvalid)
	}
	body, err := json.Marshal(struct {
		Name          string   `json:"name"`
		RunnerGroupID int64    `json:"runner_group_id"`
		Labels        []string `json:"labels"`
		WorkFolder    string   `json:"work_folder"`
	}{identity.RunnerName, runnerGroupID, []string{identity.Label}, identity.WorkFolder})
	if err != nil {
		return JITRegistration{}, fmt.Errorf("JIT encode: %w", err)
	}
	endpoint := client.repoPath(repository) + "/actions/runners/generate-jitconfig"
	status, responseBody, requestID, err := client.do(ctx, http.MethodPost, endpoint, nil, body, maxJITResponse)
	if err != nil {
		return JITRegistration{}, ambiguousTransportError(ctx, "generate JIT config")
	}
	defer Zero(responseBody)
	if status != http.StatusCreated {
		remote := &HTTPError{Operation: "generate JIT config", Status: status, RequestID: requestID}
		if status >= 400 && status < 500 {
			return JITRegistration{}, remote
		}
		return JITRegistration{}, fmt.Errorf("%w: %w", ErrAmbiguous, remote)
	}
	var response struct {
		Encoded secretBytes `json:"encoded_jit_config"`
		Runner  struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Busy   bool   `json:"busy"`
			Labels []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"labels"`
		} `json:"runner"`
	}
	if err := decodeUniqueJSON(responseBody, &response); err != nil || len(response.Encoded) < 16 || len(response.Encoded) > maxJITResponse {
		Zero(response.Encoded)
		return JITRegistration{}, fmt.Errorf("%w: JIT response is malformed", ErrAmbiguous)
	}
	if !validJITRunnerResponse(response.Runner.ID, response.Runner.Name, response.Runner.Status, response.Runner.Busy, response.Runner.Labels, identity) {
		Zero(response.Encoded)
		return JITRegistration{}, fmt.Errorf("%w: JIT runner identity or labels are invalid", ErrAmbiguous)
	}
	labels := make([]RunnerLabel, 0, len(response.Runner.Labels))
	for _, label := range response.Runner.Labels {
		labels = append(labels, RunnerLabel{Name: label.Name, Type: label.Type})
	}
	return JITRegistration{
		Secret: &JITSecret{value: []byte(response.Encoded)},
		Runner: RemoteRunner{ID: response.Runner.ID, Name: response.Runner.Name,
			Status: response.Runner.Status, Busy: response.Runner.Busy, Labels: labels},
	}, nil
}

func validJITRunnerResponse(
	id int64,
	name, status string,
	busy bool,
	labels []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	},
	identity Identity,
) bool {
	if id <= 0 || name != identity.RunnerName || status != "offline" || busy {
		return false
	}
	customNonce := 0
	for _, label := range labels {
		if label.Type == "custom" && label.Name == identity.Label {
			customNonce++
			continue
		}
		if label.Type != "read-only" || !knownDefaultRunnerLabel(label.Name) {
			return false
		}
	}
	return customNonce == 1
}

func knownDefaultRunnerLabel(label string) bool {
	switch strings.ToLower(label) {
	case "self-hosted", "linux", "x64", "arm", "arm64":
		return true
	default:
		return false
	}
}

func validIdentity(identity Identity) bool {
	if len(identity.Nonce) != 64 || !digestRE.MatchString(identity.Nonce) {
		return false
	}
	return identity.Label == "civm-jit-"+identity.Nonce &&
		identity.RunnerName == "civm-jit-"+identity.Nonce[:16] &&
		identity.WorkFolder == "_work/jit-"+identity.Nonce
}

func (client *GitHubClient) CancelRun(ctx context.Context, repository string, runID int64) error {
	if validateRepository(repository) != nil || runID <= 0 {
		return fmt.Errorf("%w: run ID is invalid", ErrInvalid)
	}
	endpoint := client.repoPath(repository) + "/actions/runs/" + strconv.FormatInt(runID, 10) + "/cancel"
	status, _, requestID, err := client.do(ctx, http.MethodPost, endpoint, nil, nil, maxAPIResponse)
	if err != nil {
		return ambiguousTransportError(ctx, "cancel run")
	}
	if status != http.StatusAccepted {
		remote := &HTTPError{Operation: "cancel run", Status: status, RequestID: requestID}
		if status == http.StatusConflict {
			return remote
		}
		return fmt.Errorf("%w: %w", ErrAmbiguous, remote)
	}
	return nil
}

func (client *GitHubClient) getJSON(
	ctx context.Context,
	operation, endpoint string,
	query url.Values,
	target any,
) error {
	status, body, requestID, err := client.do(ctx, http.MethodGet, endpoint, query, nil, maxAPIResponse)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("GitHub API %s canceled: %w", operation, ctx.Err())
		}
		return fmt.Errorf("GitHub API %s transport failed", operation)
	}
	if status != http.StatusOK {
		return &HTTPError{Operation: operation, Status: status, RequestID: requestID}
	}
	if err := decodeUniqueJSON(body, target); err != nil {
		return fmt.Errorf("GitHub API %s response is malformed", operation)
	}
	return nil
}

func (client *GitHubClient) do(
	ctx context.Context,
	method, endpoint string,
	query url.Values,
	body []byte,
	limit int64,
) (int, []byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, method, client.endpoint(endpoint, query), bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+string(client.token))
	request.Header.Set("X-GitHub-Api-Version", client.version)
	request.Header.Set("User-Agent", "civmctl-jit-dispatch/1")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return response.StatusCode, nil, safeRequestID(response.Header.Get("X-GitHub-Request-Id")), err
	}
	if int64(len(responseBody)) > limit {
		Zero(responseBody)
		return response.StatusCode, nil, safeRequestID(response.Header.Get("X-GitHub-Request-Id")), fmt.Errorf("response exceeds safety limit")
	}
	return response.StatusCode, responseBody, safeRequestID(response.Header.Get("X-GitHub-Request-Id")), nil
}

func (client *GitHubClient) repoPath(repository string) string {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || validateRepository(repository) != nil {
		return "/repos/invalid/invalid"
	}
	return "/repos/" + parts[0] + "/" + parts[1]
}

func (client *GitHubClient) endpoint(endpoint string, query url.Values) string {
	result := *client.base
	result.Path = path.Join(client.base.Path, endpoint)
	result.RawQuery = query.Encode()
	return result.String()
}

func safeRequestID(value string) string {
	if requestIDHeaderRE.MatchString(value) {
		return value
	}
	return ""
}

func ambiguousTransportError(ctx context.Context, operation string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %s canceled: %w", ErrAmbiguous, operation, ctx.Err())
	}
	return fmt.Errorf("%w: %s transport failed", ErrAmbiguous, operation)
}

func validateDispatchInputs(inputs map[string]string) error {
	if len(inputs) != 4 || !shaRE.MatchString(inputs["candidate_sha"]) ||
		validateRef(inputs["candidate_ref"]) != nil || !digestRE.MatchString(inputs["dispatch_nonce"]) ||
		inputs["runner_label"] != "civm-jit-"+inputs["dispatch_nonce"] {
		return ErrInvalid
	}
	for _, key := range []string{"candidate_sha", "candidate_ref", "dispatch_nonce", "runner_label"} {
		if _, exists := inputs[key]; !exists {
			return ErrInvalid
		}
	}
	return nil
}
