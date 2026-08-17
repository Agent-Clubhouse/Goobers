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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func seedReviewThreadResolutionRun(t *testing.T, root, runID, responses string) {
	t.Helper()
	brief := reviewThreadsBrief()
	brief.GatherReviewThreads = &apiv1.RemediationReviewThreads{
		InlineComments: []apiv1.RemediationInlineComment{
			{ID: 101, ThreadID: "PRRT_addressed", Body: "fix", Path: "a.go", Integrity: apiv1.IntegrityUnapproved},
			{ID: 201, ThreadID: "PRRT_obsolete", Body: "old", Path: "b.go", Integrity: apiv1.IntegrityUnapproved},
			{ID: 301, ThreadID: "PRRT_blocked", Body: "blocked", Path: "c.go", Integrity: apiv1.IntegrityUnapproved},
		},
		Reviews: []apiv1.RemediationNativeReview{},
	}
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(brief)
	if _, err := run.RecordArtifact(runID+":gather-issue-context/result", data); err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess), Outputs: map[string]any{threadResponsesOutput: responses}},
		{Type: journal.EventStageFinished, Stage: "push-remediated", Attempt: 1, Status: string(apiv1.ResultSuccess), Outputs: map[string]any{pushRemediatedPublishedOutput: "true"}},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveReviewThreadsRepliesBeforeResolvingAndReturnsUnresolvedCount(t *testing.T) {
	const runID = "resolve-threads"
	root := initDemo(t)
	responses := `[
		{"threadId":"PRRT_addressed","disposition":"addressed","detail":"added synchronization"},
		{"threadId":"PRRT_obsolete","disposition":"obsolete","detail":"code was removed"},
		{"threadId":"PRRT_blocked","disposition":"blocked","detail":"needs maintainer input"}
	]`
	seedReviewThreadResolutionRun(t, root, runID, responses)

	type threadState struct {
		rootID   int64
		resolved bool
		replies  []map[string]any
	}
	threads := map[string]*threadState{
		"PRRT_addressed": {rootID: 101},
		"PRRT_obsolete":  {rootID: 201},
		"PRRT_blocked":   {rootID: 301},
	}
	nextID := int64(400)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77":
			_, _ = w.Write([]byte(`{"number":77,"state":"open","head":{"ref":"work","sha":"published-sha"},"base":{"ref":"main","sha":"base-sha"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77/reviews":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77/comments":
			var comments []map[string]any
			for _, state := range threads {
				comments = append(comments, map[string]any{"id": state.rootID, "body": "finding", "path": "x.go"})
				comments = append(comments, state.replies...)
			}
			_ = json.NewEncoder(w).Encode(comments)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/replies"):
			parts := strings.Split(r.URL.Path, "/")
			rootID, _ := strconv.ParseInt(parts[len(parts)-2], 10, 64)
			var request map[string]string
			_ = json.NewDecoder(r.Body).Decode(&request)
			for _, state := range threads {
				if state.rootID == rootID {
					nextID++
					reply := map[string]any{"id": nextID, "body": request["body"], "path": "x.go", "in_reply_to_id": rootID}
					state.replies = append(state.replies, reply)
					_ = json.NewEncoder(w).Encode(reply)
					return
				}
			}
			t.Fatalf("reply posted to unknown root %d", rootID)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var request struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if strings.Contains(request.Query, "mutation") {
				id := request.Variables["threadId"].(string)
				state := threads[id]
				if len(state.replies) == 0 {
					t.Fatalf("thread %s resolved before its reply was visible", id)
				}
				state.resolved = true
				_, _ = w.Write([]byte(`{"data":{"resolveReviewThread":{"thread":{"id":"` + id + `","isResolved":true}}}}`))
				return
			}
			var nodes []map[string]any
			for id, state := range threads {
				commentNodes := []map[string]any{{"databaseId": state.rootID}}
				for _, reply := range state.replies {
					commentNodes = append(commentNodes, map[string]any{"databaseId": reply["id"]})
				}
				nodes = append(nodes, map[string]any{
					"id": id, "isResolved": state.resolved, "isOutdated": false, "path": "x.go",
					"comments": map[string]any{"nodes": commentNodes},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
				"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false},
			}}}}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	previousProvider := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		provider := providers.NewGitHubProvider(token, opts...)
		provider.BaseURL = server.URL
		return provider
	}
	t.Cleanup(func() { newGitHubProvider = previousProvider })
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	dir := t.TempDir()
	t.Chdir(dir)

	if code, stdout, stderr := runArgs(t, "resolve-review-threads", root); code != 0 {
		t.Fatalf("resolve-review-threads: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, resolveReviewThreadsResultFile))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result[unresolvedReviewThreadCountOutput] != "2" || result["publishedHeadSha"] != "published-sha" {
		t.Fatalf("result = %v, want unresolved count 2 at published-sha", result)
	}
	if !threads["PRRT_addressed"].resolved || threads["PRRT_obsolete"].resolved || threads["PRRT_blocked"].resolved {
		t.Fatalf("thread resolution states = %+v", threads)
	}
}

func TestValidateThreadResponsesRejectsMissingAndInventedIDs(t *testing.T) {
	threads := map[string]gatheredReviewThread{"PRRT_1": {ID: "PRRT_1", CommentID: 1}}
	for _, raw := range []string{
		`[]`,
		`[{"threadId":"PRRT_invented","disposition":"addressed","detail":"done"}]`,
	} {
		if _, err := validateThreadResponses(threads, raw); err == nil {
			t.Fatalf("validateThreadResponses(%s) succeeded, want fail-closed error", raw)
		}
	}
}

func TestGatheredLiveReviewThreadsRequiresStableIDs(t *testing.T) {
	for _, comment := range []apiv1.RemediationInlineComment{
		{ID: 1},
		{ThreadID: "PRRT_1"},
	} {
		section := &apiv1.RemediationReviewThreads{
			InlineComments: []apiv1.RemediationInlineComment{comment},
		}
		if _, err := gatheredLiveReviewThreads(section); err == nil {
			t.Fatalf("gatheredLiveReviewThreads(%+v) succeeded without stable IDs", comment)
		}
	}
}

func TestResolveReviewThreadsDoesNotResolveWhenReplyFails(t *testing.T) {
	const runID = "reply-fails"
	root := initDemo(t)
	seedReviewThreadResolutionRun(t, root, runID, `[
		{"threadId":"PRRT_addressed","disposition":"addressed","detail":"fixed"},
		{"threadId":"PRRT_obsolete","disposition":"obsolete","detail":"removed"},
		{"threadId":"PRRT_blocked","disposition":"blocked","detail":"waiting"}
	]`)
	resolveAttempted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77":
			_, _ = w.Write([]byte(`{"number":77,"state":"open","head":{"ref":"work","sha":"published-sha"},"base":{"ref":"main","sha":"base-sha"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77/reviews":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77/comments":
			_, _ = w.Write([]byte(`[
				{"id":101,"body":"fix","path":"a.go"},
				{"id":201,"body":"old","path":"b.go"},
				{"id":301,"body":"blocked","path":"c.go"}
			]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/replies"):
			http.Error(w, "reply failed", http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var request struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if strings.Contains(request.Query, "mutation") {
				resolveAttempted = true
				t.Fatal("resolution attempted after reply failure")
			}
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
				{"id":"PRRT_addressed","isResolved":false,"isOutdated":false,"path":"a.go","comments":{"nodes":[{"databaseId":101}]}},
				{"id":"PRRT_obsolete","isResolved":false,"isOutdated":false,"path":"b.go","comments":{"nodes":[{"databaseId":201}]}},
				{"id":"PRRT_blocked","isResolved":false,"isOutdated":false,"path":"c.go","comments":{"nodes":[{"databaseId":301}]}}
			],"pageInfo":{"hasNextPage":false}}}}}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	previousProvider := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		provider := providers.NewGitHubProvider(token, opts...)
		provider.BaseURL = server.URL
		return provider
	}
	t.Cleanup(func() { newGitHubProvider = previousProvider })
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Chdir(t.TempDir())

	if code, _, _ := runArgs(t, "resolve-review-threads", root); code == 0 {
		t.Fatal("resolve-review-threads succeeded despite reply failure")
	}
	if resolveAttempted {
		t.Fatal("resolution was attempted despite reply failure")
	}
}
