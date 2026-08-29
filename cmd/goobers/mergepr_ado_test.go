package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// adoMergePRServer is a fake Azure DevOps REST surface for the merge-pr land:
// the single-PR GET (PollPullRequest + the land's own getPullRequestDetail),
// the policy-evaluations read (CheckState), the policy-configurations read
// (DetectMergePolicy), and the completion PATCH (MergePullRequest).
type adoMergePRServer struct {
	getCalls   int64
	patchCalls int64
	evalCalls  int64
	cfgCalls   int64
	patchBody  atomic.Value // map[string]interface{}
}

// newADOMergePRServer stands up the fake ADO endpoints for org/project/repo,
// polling for pull request 359. The PR is a clean pass: active, not draft, head
// pinned to headSHA, base pinned to baseSHA, its body closing work item 1456,
// and a single approved blocking Build policy (→ CheckState passing). No branch
// policy configuration is returned, so DetectMergePolicy resolves to a direct
// merge and the completion PATCH lands it.
func newADOMergePRServer(t *testing.T, headSHA, baseSHA string) (*httptest.Server, *adoMergePRServer) {
	t.Helper()
	state := &adoMergePRServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/myorg/myproject/_apis/git/repositories/myrepo/pullrequests/359", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			atomic.AddInt64(&state.getCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pullRequestId":         359,
				"url":                   "https://dev.azure.com/myorg/_git/myrepo/pullrequest/359",
				"status":                "active",
				"title":                 "Wire ADO merge dispatch",
				"isDraft":               false,
				"sourceRefName":         "refs/heads/goobers/tb-ado-implementation/wire-merge",
				"targetRefName":         "refs/heads/main",
				"lastMergeSourceCommit": map[string]string{"commitId": headSHA},
				"lastMergeTargetCommit": map[string]string{"commitId": baseSHA},
				"description":           "Implements the merge wiring.\n\nFixes #1456",
				"repository": map[string]any{
					"id":   "repo-guid",
					"name": "myrepo",
					"project": map[string]string{
						"id":   "proj-guid",
						"name": "myproject",
					},
				},
			})
		case http.MethodPatch:
			atomic.AddInt64(&state.patchCalls, 1)
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			state.patchBody.Store(body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pullRequestId":   359,
				"status":          "completed",
				"mergeStatus":     "succeeded",
				"lastMergeCommit": map[string]string{"commitId": "mergedsha1"},
			})
		default:
			t.Errorf("unexpected method %s on pullrequests/359", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/myorg/myproject/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&state.evalCalls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{
				{
					"status": "approved",
					"configuration": map[string]any{
						"id":         1,
						"isEnabled":  true,
						"isBlocking": true,
						"type":       map[string]string{"displayName": "Build"},
					},
				},
			},
		})
	})
	mux.HandleFunc("/myorg/myproject/_apis/policy/configurations", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&state.cfgCalls, 1)
		// No blocking branch policy → DetectMergePolicy = direct merge.
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, state
}

// adoMergePREnv wires a runnable merge-pr CLI invocation routed to Azure
// DevOps: instance root, run/workflow identity, the routed ADO repo env, the
// ado:pr:complete grant (unless withoutGrant), and the declared inputs. It
// overrides newADOProviderForStage to point every constructed provider at
// serverURL. Returns (instanceRoot, workDir) — workDir is cwd, where the result
// file lands.
func adoMergePREnv(t *testing.T, serverURL string, withoutGrant bool, inputs map[string]string) (string, string) {
	t.Helper()
	instanceRoot := initDemo(t)

	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderADO))
	t.Setenv(executor.RepoOwnerEnvVar, "myorg")
	t.Setenv(executor.RepoProjectEnvVar, "myproject")
	t.Setenv(executor.RepoNameEnvVar, "myrepo")

	t.Setenv("GOOBERS_RUN_ID", "run-merge-ado-1")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	if !withoutGrant {
		// GOOBERS_CRED_ADO_PR_COMPLETE — the injected ado:pr:complete grant.
		t.Setenv(executor.CredentialEnvVar("ado:pr:complete"), "ado-complete-token")
	}
	for k, v := range inputs {
		t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(k), v)
	}

	prev := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(
			routed.Owner,
			routed.Project,
			"token",
			func(p *providers.ADOProvider) { p.BaseURL = serverURL },
		), nil
	}
	t.Cleanup(func() { newADOProviderForStage = prev })

	workDir := t.TempDir()
	t.Chdir(workDir)
	return instanceRoot, workDir
}

// TestMergePRDispatchesToADOAndLandsWithoutVerdictComment is the load-bearing
// ADO-branch proof: a clean ADO pass (green Build policy, no branch policy, SHA
// pins matching) reaches the actual land through the dispatcher and merges —
// crucially, even though ADO PollPullRequest returns an empty CommentsSince,
// which would make structuredMergeCommitMessage error and hard-fail the stage
// on the GitHub path. The commit message is built directly from the PR's title
// + closing refs (the single-hard-blocker fix), and no verdictAuthor is
// supplied (ADO does not need one).
func TestMergePRDispatchesToADOAndLandsWithoutVerdictComment(t *testing.T) {
	server, state := newADOMergePRServer(t, "headsha1", "basesha1")
	root, dir := adoMergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "359",
		"verdict":    "pass",
		"headSha":    "headsha1",
		"baseSha":    "basesha1",
		// deliberately no verdictAuthor: ADO must not require it.
	})

	code, stdout, stderr := runArgs(t, "merge-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readMergeResult(t, dir)
	if merged, _ := result["merged"].(bool); !merged {
		t.Fatalf("result = %+v, want merged=true", result)
	}
	if result["mergeSha"] != "mergedsha1" {
		t.Fatalf("result = %+v, want mergeSha=mergedsha1", result)
	}
	if result["selectedNumber"] != "359" {
		t.Fatalf("result = %+v, want selectedNumber=359", result)
	}
	// Branch cleanup is gated OFF on ADO: no branchCleanup field is emitted.
	if _, ok := result["branchCleanup"]; ok {
		t.Fatalf("result = %+v, want no branchCleanup on ADO", result)
	}
	if atomic.LoadInt64(&state.patchCalls) != 1 {
		t.Fatalf("completion PATCH called %d times, want 1", state.patchCalls)
	}
	// The land carried the commit message the ADO bypass assembled from the PR
	// body's closing ref — NOT a verdict rationale (there is no verdict comment).
	body, _ := state.patchBody.Load().(map[string]interface{})
	opts, _ := body["completionOptions"].(map[string]interface{})
	if opts == nil {
		t.Fatalf("PATCH body = %+v, want completionOptions", body)
	}
	if got := opts["mergeCommitMessage"]; got != "Closes #1456" {
		t.Fatalf("mergeCommitMessage = %q, want %q", got, "Closes #1456")
	}
	if got := opts["mergeStrategy"]; got != "squash" {
		t.Fatalf("mergeStrategy = %q, want squash", got)
	}
}

// TestMergePRADORequiresCompletionCapability proves the merge/completion
// authority on ADO is gated on the dedicated ado:pr:complete capability: with
// the grant absent, merge-pr fails closed BEFORE constructing any provider —
// the fake ADO server is never touched — so a stage carrying only ado:pr:write
// can never complete a pull request.
func TestMergePRADORequiresCompletionCapability(t *testing.T) {
	server, state := newADOMergePRServer(t, "headsha1", "basesha1")
	root, _ := adoMergePREnv(t, server.URL, true, map[string]string{
		"pullNumber": "359",
		"verdict":    "pass",
		"headSha":    "headsha1",
		"baseSha":    "basesha1",
	})

	code, _, stderr := runArgs(t, "merge-pr", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail-closed); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "ADO_PR_COMPLETE") {
		t.Fatalf("stderr = %q, want the missing ado:pr:complete credential named", stderr)
	}
	if n := atomic.LoadInt64(&state.getCalls) + atomic.LoadInt64(&state.patchCalls); n != 0 {
		t.Fatalf("ADO server received %d PR requests, want 0 (must fail before any provider call)", n)
	}
}

// TestADOMergeCommitMessageBypassesVerdictLookup pins the single-hard-blocker
// fix at the unit level: on a poll with an EMPTY CommentsSince (exactly what ADO
// PollPullRequest returns), structuredMergeCommitMessage errors, while
// adoMergeCommitMessage succeeds — assembling the title + "Closes #N" closing
// refs directly from the PR, with no verdict comment required.
func TestADOMergeCommitMessageBypassesVerdictLookup(t *testing.T) {
	poll := providers.PullRequestPollResult{
		Title: "Wire ADO merge dispatch",
		Body:  "Implements the merge wiring.\n\nFixes #1456",
		// CommentsSince deliberately empty — the ADO condition.
	}

	if _, _, err := structuredMergeCommitMessage(poll, "goobers"); err == nil {
		t.Fatal("structuredMergeCommitMessage: want error on empty CommentsSince, got nil")
	}

	title, message, err := adoMergeCommitMessage(poll)
	if err != nil {
		t.Fatalf("adoMergeCommitMessage: unexpected error %v", err)
	}
	if title != "Wire ADO merge dispatch" {
		t.Fatalf("title = %q, want the PR title", title)
	}
	if message != "Closes #1456" {
		t.Fatalf("message = %q, want %q", message, "Closes #1456")
	}

	// An empty title is still a business error, matching the GitHub assembly.
	if _, _, err := adoMergeCommitMessage(providers.PullRequestPollResult{Title: "   "}); err == nil {
		t.Fatal("adoMergeCommitMessage: want error on empty title, got nil")
	}
}
