package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// TestBacklogQueryRateLimitedWritesTypedErrorResult is #614's CLI-level
// acceptance, reproducing the live incident end to end: every GitHub request
// 403s with x-ratelimit-remaining: 0 and a reset far beyond the wait budget.
// The stage must exit 1 AND leave the declared result file behind carrying
// the structured errorCode fields — so internal/executor/shell.go journals
// the failure as github_rate_limited (with the reset time), not as the
// missing_result_file that made the incident a raw-stderr archaeology
// exercise. Deliberately not a real-network test: the fake server stands in
// for api.github.com via the newGitHubProvider seam.
func TestBacklogQueryRateLimitedWritesTypedErrorResult(t *testing.T) {
	root := initDemo(t)
	reset := time.Now().Add(45 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		http.Error(w, `{"message":"API rate limit exceeded for user ID 1669494.","documentation_url":"https://docs.github.com/en/rest/using-the-rest-api/getting-started-with-the-rest-api#rate-limiting"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = srv.URL })...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-614")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderrOut := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("backlog-query under rate limit: code = %d, stderr = %q, want 1", code, stderrOut)
	}
	if !strings.Contains(stderrOut, providers.ErrorCodeRateLimited) {
		t.Fatalf("stderr = %q, want the typed %s cause visible to an operator", stderrOut, providers.ErrorCodeRateLimited)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json (the structured-error channel to shell.go): %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if out[executor.OutputErrorCode] != providers.ErrorCodeRateLimited {
		t.Fatalf("errorCode = %v, want %s", out[executor.OutputErrorCode], providers.ErrorCodeRateLimited)
	}
	if out[executor.OutputErrorRetryable] != true {
		t.Fatalf("errorRetryable = %v, want true", out[executor.OutputErrorRetryable])
	}
	msg, _ := out[executor.OutputErrorMessage].(string)
	if !strings.Contains(msg, "list work items") {
		t.Fatalf("errorMessage = %q, want the failing operation named", msg)
	}
	if rst, _ := out["rateLimitReset"].(string); rst == "" {
		t.Fatalf("rateLimitReset missing from %v, want the reset timestamp for the journal", out)
	}
}
