package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// TestBacklogQueryServerErrorWritesTypedErrorResult is #711's core CLI-level
// acceptance, reproducing the live #705/#711 incident evidence end to end:
// every GitHub request returns a 503 with GitHub's actual "Unicorn!" HTML
// load-shedding page as the body — no rate-limit headers, so this must NOT
// be misclassified as github_rate_limited. Before #711 this fell all the
// way through to the generic missing_result_file, hiding the real cause;
// now backlog-query's declared result file carries github_server_error with
// the status inline, retryable, so a resumed run doesn't burn its whole
// attempt budget on one transient GitHub blip.
func TestBacklogQueryServerErrorWritesTypedErrorResult(t *testing.T) {
	root := initDemo(t)
	const unicornPage = `<html><head><title>500 Internal Server Error</title></head><body><h2>Unicorn! You've been visited by a horrible server error.</h2></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(unicornPage))
	}))
	t.Cleanup(srv.Close)

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		// Keep this test focused on failProviderStage's classification of the
		// final give-up error; the fake server always 503s.
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = srv.URL }, providers.WithMaxTransientRetries(0))...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-711-server-error")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderrOut := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("backlog-query under a 503: code = %d, stderr = %q, want 1", code, stderrOut)
	}
	if !strings.Contains(stderrOut, "status 503") {
		t.Fatalf("stderr = %q, want the actual status visible to an operator, not a generic failure", stderrOut)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json (the structured-error channel to shell.go): %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if out[executor.OutputErrorCode] != errorCodeServerError {
		t.Fatalf("errorCode = %v, want %s — a 503/HTML load-shedding response must not collapse into missing_result_file or github_rate_limited", out[executor.OutputErrorCode], errorCodeServerError)
	}
	if out[executor.OutputErrorRetryable] != true {
		t.Fatalf("errorRetryable = %v, want true — 5xx is transient", out[executor.OutputErrorRetryable])
	}
	msg, _ := out[executor.OutputErrorMessage].(string)
	if !strings.Contains(msg, "503") {
		t.Fatalf("errorMessage = %q, want the HTTP status inline", msg)
	}
	if _, hasRateLimitReset := out["rateLimitReset"]; hasRateLimitReset {
		t.Fatalf("out = %v, must not carry a rateLimitReset field — this is not a rate limit", out)
	}
}

// TestBacklogQueryAuthFailureWritesTypedErrorResult is #711's auth-failure
// AC: a real permission 401 (no Retry-After, no X-RateLimit-Remaining: 0 —
// so isRateLimited correctly does not intercept it) must classify as
// github_auth_failed and non-retryable, distinct from every rate-limit and
// server-error shape.
func TestBacklogQueryAuthFailureWritesTypedErrorResult(t *testing.T) {
	root := initDemo(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = srv.URL })...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-711-auth-failure")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderrOut := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("backlog-query under a 401: code = %d, stderr = %q, want 1", code, stderrOut)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if out[executor.OutputErrorCode] != errorCodeAuthFailed {
		t.Fatalf("errorCode = %v, want %s", out[executor.OutputErrorCode], errorCodeAuthFailed)
	}
	if out[executor.OutputErrorRetryable] != false {
		t.Fatalf("errorRetryable = %v, want false — retrying the same bad credential cannot succeed", out[executor.OutputErrorRetryable])
	}
}

// TestBacklogQueryNetworkErrorWritesTypedErrorResult is #711's network AC: a
// transport-level failure (here, a connection refused — nothing listening
// on the target port, standing in for a DNS blip/reset/timeout) that
// exhausts send()'s in-request retry budget must classify as network_error
// and retryable, not the generic missing_result_file the old plain
// "send request: ..." error left the operator with.
func TestBacklogQueryNetworkErrorWritesTypedErrorResult(t *testing.T) {
	root := initDemo(t)

	// A real httptest.Server, closed before use: guarantees "connection
	// refused" on the OS-assigned port that was briefly bound, fast and
	// deterministic — no DNS or firewall dependency an unreachable public
	// host would carry.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachable := srv.URL
	srv.Close()

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts,
			func(p *providers.GitHubProvider) { p.BaseURL = unreachable },
			providers.WithMaxTransientRetries(0),
		)...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-711-network-error")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, _ := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("backlog-query against an unreachable host: code = %d, want 1", code)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if out[executor.OutputErrorCode] != errorCodeNetwork {
		t.Fatalf("errorCode = %v, want %s", out[executor.OutputErrorCode], errorCodeNetwork)
	}
	if out[executor.OutputErrorRetryable] != true {
		t.Fatalf("errorRetryable = %v, want true — a network blip is unrelated to the request's content", out[executor.OutputErrorRetryable])
	}
}
