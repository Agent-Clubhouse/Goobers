package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
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

func TestBacklogQueryFatalProviderPathsKeepGenericEnvelope(t *testing.T) {
	type failureCase struct {
		name       string
		operation  string
		args       []string
		resultFile string
		setup      func(*testing.T, string, *fakeGitHubServer)
		match      func(*http.Request) bool
	}

	issueCollection := "/repos/your-org/your-repo/issues"
	pullCollection := "/repos/your-org/your-repo/pulls"
	cases := []failureCase{
		{
			name:       "metadata reconciliation",
			operation:  "reconcile backlog metadata",
			args:       []string{"--reconcile"},
			resultFile: "backlog-reconciliation.json",
			setup: func(t *testing.T, _ string, server *fakeGitHubServer) {
				server.addIssue(7, "Contradictory", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
			},
			match: func(r *http.Request) bool {
				return r.Method == http.MethodPost && r.URL.Path == issueCollection+"/7/comments"
			},
		},
		{
			name:       "open pull request listing",
			operation:  "list open pull requests",
			args:       []string{"--claim"},
			resultFile: "claimed-item.json",
			setup: func(t *testing.T, _ string, server *fakeGitHubServer) {
				server.addIssue(7, "Candidate", "goobers:approved")
				t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
			},
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == pullCollection
			},
		},
		{
			name:       "closed pull request reconciliation",
			operation:  "reconcile closed pull requests",
			args:       []string{"--claim"},
			resultFile: "claimed-item.json",
			setup: func(t *testing.T, _ string, server *fakeGitHubServer) {
				server.addIssue(7, "Retry implementation", "goobers:approved", providers.LabelReady, inReviewStatusLabel)
				server.addOpenPR(101, "goobers/implementation/prior-run", "main", "head", "base", false, nil, nil)
				server.setPRClosed(101)
				server.addComment(7, implementationInReviewComment("https://github.com/your-org/your-repo/pull/101"))
				t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
			},
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == pullCollection+"/101"
			},
		},
		{
			name:       "work item listing",
			operation:  "list work items",
			args:       []string{"--claim"},
			resultFile: "claimed-item.json",
			setup:      func(*testing.T, string, *fakeGitHubServer) {},
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet &&
					r.URL.Path == issueCollection &&
					r.URL.Query().Get("state") == "open"
			},
		},
		{
			name:       "blocked dependency recheck listing",
			operation:  "list blocked items for dependency recheck",
			args:       []string{"--claim"},
			resultFile: "claimed-items.json",
			setup: func(t *testing.T, _ string, server *fakeGitHubServer) {
				server.addIssue(7, "Blocked item", "goobers:approved", blockedOnSiblingLabel)
				configureCurationResweep(t, "2", "1", "24h")
			},
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet &&
					r.URL.Path == issueCollection &&
					strings.Contains(r.URL.Query().Get("labels"), blockedOnSiblingLabel)
			},
		},
		{
			name:       "ready item re-sweep listing",
			operation:  "list ready items for re-sweep",
			args:       []string{"--claim"},
			resultFile: "claimed-items.json",
			setup: func(t *testing.T, _ string, server *fakeGitHubServer) {
				server.addIssue(7, "Ready item", "goobers:approved", providers.LabelReady)
				configureCurationResweep(t, "2", "2", "24h")
			},
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet &&
					r.URL.Path == issueCollection &&
					strings.Contains(r.URL.Query().Get("labels"), providers.LabelReady)
			},
		},
		{
			name:       "ready label transitions",
			operation:  "read ready-label transitions",
			args:       []string{"--claim"},
			resultFile: "claimed-item.json",
			setup: func(t *testing.T, _ string, server *fakeGitHubServer) {
				server.addIssue(7, "Ready item", "goobers:approved", providers.LabelReady)
				t.Setenv("GOOBERS_INPUT_REQUIRELABELS", providers.LabelReady)
			},
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == issueCollection+"/7/events"
			},
		},
		{
			name:       "claimed item staleness",
			operation:  "compute claimed-item staleness",
			args:       []string{"--claim"},
			resultFile: "claimed-items.json",
			setup: func(t *testing.T, _ string, server *fakeGitHubServer) {
				server.addIssue(7, "Curation candidate", "goobers:approved")
				t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
				t.Setenv("GOOBERS_INPUT_CURATION", "true")
				t.Setenv("GOOBERS_INPUT_MAXITEMS", "2")
			},
			match: func() func(*http.Request) bool {
				var calls atomic.Int32
				return func(r *http.Request) bool {
					return r.Method == http.MethodGet &&
						r.URL.Path == issueCollection+"/7/comments" &&
						calls.Add(1) == 3
				}
			}(),
		},
		{
			name:       "read-only re-sweep staleness",
			operation:  "compute read-only re-sweep staleness",
			args:       []string{"--claim"},
			resultFile: "claimed-items.json",
			setup: func(t *testing.T, _ string, server *fakeGitHubServer) {
				server.addIssue(7, "In-flight item", "goobers:approved", providers.LabelReady, inReviewStatusLabel)
				configureCurationResweep(t, "2", "2", "24h")
			},
			match: func() func(*http.Request) bool {
				var calls atomic.Int32
				return func(r *http.Request) bool {
					return r.Method == http.MethodGet &&
						r.URL.Path == "/user" &&
						calls.Add(1) == 2
				}
			}(),
		},
		{
			name:       "release cleanup",
			operation:  "release backlog claims",
			args:       []string{"--release"},
			resultFile: "backlog-release.json",
			setup: func(t *testing.T, root string, server *fakeGitHubServer) {
				server.addIssue(7, "Claimed item", providers.LabelClaimed)
				server.addComment(7, "goobers-claim: run=run-1760-provider-error\n\nClaimed for audit.")
				ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
				if err != nil {
					t.Fatal(err)
				}
				if ok, _, err := ledger.Claim("7", "run-1760-provider-error", "implementation", DefaultClaimLease); err != nil || !ok {
					t.Fatalf("seed release claim: ok=%v err=%v", ok, err)
				}
			},
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == issueCollection+"/7/comments"
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1760-provider-error")
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
			t.Setenv("GOOBERS_INPUT_RESULTFILE", tt.resultFile)
			tt.setup(t, root, server)

			baseHandler := server.server.Config.Handler
			var injected atomic.Bool
			server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !injected.Load() && tt.match(r) {
					injected.Store(true)
					http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
					return
				}
				baseHandler.ServeHTTP(w, r)
			})

			workDir := t.TempDir()
			t.Chdir(workDir)
			commandArgs := append([]string{"backlog-query"}, tt.args...)
			commandArgs = append(commandArgs, root)
			code, _, stderrOut := runArgs(t, commandArgs...)
			if code != 1 {
				t.Fatalf("%s under a 422: code = %d, stderr = %q, want 1", tt.operation, code, stderrOut)
			}
			if !injected.Load() {
				t.Fatalf("%s did not reach the audited provider request", tt.operation)
			}
			assertGenericProviderErrorResult(t, filepath.Join(workDir, tt.resultFile), tt.operation)
		})
	}
}

func assertGenericProviderErrorResult(t *testing.T, path, operation string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	if out[executor.OutputErrorCode] != errorCodeProvider {
		t.Fatalf("errorCode = %v, want %s", out[executor.OutputErrorCode], errorCodeProvider)
	}
	if out[executor.OutputErrorRetryable] != false {
		t.Fatalf("errorRetryable = %v, want false", out[executor.OutputErrorRetryable])
	}
	message, _ := out[executor.OutputErrorMessage].(string)
	if !strings.Contains(message, operation) || !strings.Contains(message, "status 422") {
		t.Fatalf("errorMessage = %q, want operation %q and provider status", message, operation)
	}
	if out["integrity"] != string(apiintegrity.Unapproved) {
		t.Fatalf("%s integrity = %v, want unapproved", path, out["integrity"])
	}
	if len(out) != 4 {
		t.Fatalf("%s = %v, want only the generic provider failure envelope and integrity", path, out)
	}
}
