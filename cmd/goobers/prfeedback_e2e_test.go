package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// feedbackWorld is one provider's fake PR #77 with a single human review
// thread, whose head and feedback a test changes between stages — the chaos
// the soak injects: human comments, replies and pushes mid-remediation.
type feedbackWorld struct {
	root, runID string
	threadID    string
	setHead     func(head string)
	addComment  func(body string)
	editThread  func(body string)
	replies     func() int
	resolved    func() bool
}

func newFeedbackWorld(t *testing.T, kind providers.ProviderKind) feedbackWorld {
	t.Helper()
	switch kind {
	case providers.ProviderGitHub:
		return newGitHubFeedbackWorld(t)
	case providers.ProviderGitea:
		return newGiteaFeedbackWorld(t)
	default:
		return newADOFeedbackWorld(t)
	}
}

// githubFeedbackFake serves every read gather-review-threads, pr-claim and
// resolve-review-threads make on GitHub, plus the reply and resolve writes.
type githubFeedbackFake struct {
	mu       sync.Mutex
	head     string
	rootBody string
	resolved bool
	replies  []map[string]any
	general  []map[string]any
	nextID   int64
}

func (f *githubFeedbackFake) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77":
			writeFakeJSON(w, map[string]any{"number": 77, "state": "open", "head": map[string]any{"ref": "work", "sha": f.head}, "base": map[string]any{"ref": "main", "sha": "base-sha"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77/reviews":
			writeFakeJSON(w, []any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/issues/77/comments":
			writeFakeJSON(w, f.general)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls/77/comments":
			comments := []map[string]any{{"id": 101, "body": f.rootBody, "path": "a.go", "user": map[string]any{"login": "reviewer"}}}
			writeFakeJSON(w, append(comments, f.replies...))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/replies"):
			var request map[string]string
			_ = json.NewDecoder(r.Body).Decode(&request)
			f.nextID++
			reply := map[string]any{"id": f.nextID, "body": request["body"], "path": "a.go", "in_reply_to_id": 101, "user": map[string]any{"login": "goobers-bot"}}
			f.replies = append(f.replies, reply)
			writeFakeJSON(w, reply)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			f.serveGraphQL(w, r)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func (f *githubFeedbackFake) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	if strings.Contains(request.Query, "mutation") {
		f.resolved = true
		_, _ = w.Write([]byte(`{"data":{"resolveReviewThread":{"thread":{"id":"PRRT_1","isResolved":true}}}}`))
		return
	}
	nodes := []map[string]any{{"databaseId": 101}}
	for _, reply := range f.replies {
		nodes = append(nodes, map[string]any{"databaseId": reply["id"]})
	}
	writeFakeJSON(w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
		"nodes":    []map[string]any{{"id": "PRRT_1", "isResolved": f.resolved, "isOutdated": false, "path": "a.go", "comments": map[string]any{"nodes": nodes}}},
		"pageInfo": map[string]any{"hasNextPage": false},
	}}}}})
}

func newGitHubFeedbackWorld(t *testing.T) feedbackWorld {
	fake := &githubFeedbackFake{head: revisionSelectedSHA, rootBody: "Guard this write.", nextID: 500}
	server := fake.serve(t)
	t.Cleanup(server.Close)
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		provider := providers.NewGitHubProvider(token, opts...)
		provider.BaseURL = server.URL
		return provider
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	root := initDemo(t)
	const runID = "run-feedback-github"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	seedRevisionClaim(t, root, prClaimTestRepo(), runID)
	return feedbackWorld{
		root: root, runID: runID, threadID: "PRRT_1",
		setHead: func(head string) { fake.mu.Lock(); fake.head = head; fake.mu.Unlock() },
		addComment: func(body string) {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			fake.nextID++
			fake.general = append(fake.general, map[string]any{"id": fake.nextID, "body": body, "user": map[string]any{"login": "human", "type": "User"}})
		},
		editThread: func(body string) { fake.mu.Lock(); fake.rootBody = body; fake.mu.Unlock() },
		replies:    func() int { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.replies) },
		resolved:   func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return fake.resolved },
	}
}

func newGiteaFeedbackWorld(t *testing.T) feedbackWorld {
	var mu sync.Mutex
	head, rootBody := revisionSelectedSHA, "Guard this write."
	general := []map[string]any{}
	next := int64(500)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		base := "/api/v1/repos/your-org/your-repo/"
		switch strings.TrimPrefix(r.URL.Path, base) {
		case "pulls/77":
			writeFakeJSON(w, map[string]any{"number": 77, "state": "open", "head": map[string]any{"ref": "work", "sha": head}, "base": map[string]any{"ref": "main", "sha": "base-sha"}})
		case "pulls/77/reviews":
			if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
				writeFakeJSON(w, []any{})
				return
			}
			writeFakeJSON(w, []map[string]any{{"id": 7, "body": "", "state": "COMMENT", "user": map[string]any{"login": "reviewer"}}})
		case "pulls/77/reviews/7/comments":
			writeFakeJSON(w, []map[string]any{{"id": 101, "body": rootBody, "path": "a.go", "user": map[string]any{"login": "reviewer"}}})
		case "issues/77/comments":
			writeFakeJSON(w, general)
		default:
			t.Errorf("unexpected Gitea request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	root := initDemo(t)
	configureRemediationGitea(t, root, server.URL)
	const runID = "run-feedback-gitea"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "gitea-token")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitea))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	seedRevisionClaim(t, root, providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "your-org", Name: "your-repo"}, runID)
	return feedbackWorld{
		root: root, runID: runID, threadID: "gitea-review-7",
		setHead: func(h string) { mu.Lock(); head = h; mu.Unlock() },
		addComment: func(body string) {
			mu.Lock()
			defer mu.Unlock()
			next++
			general = append(general, map[string]any{"id": next, "body": body, "user": map[string]any{"login": "human"}})
		},
		editThread: func(body string) { mu.Lock(); rootBody = body; mu.Unlock() },
	}
}

func newADOFeedbackWorld(t *testing.T) feedbackWorld {
	const runID = "run-feedback-ado"
	root, repo := adoReviewThreadsStageFixture(t, runID)
	fake := &fakeADOReviewThreads{t: t, head: revisionSelectedSHA, threads: map[int]*adoReviewThread{
		5: {status: "active", path: "a.go", root: "Guard this write."},
	}}
	server := fake.server(repo, revisionSelectedSHA)
	t.Cleanup(server.Close)
	routeADOStageProvider(t, server.URL)
	seedRevisionClaim(t, root, repo, runID)
	humanReplies := 0
	return feedbackWorld{
		root: root, runID: runID, threadID: "77/5",
		setHead: func(head string) { fake.mu.Lock(); fake.head = head; fake.mu.Unlock() },
		addComment: func(body string) {
			// A human reply on the thread: ADO surfaces it both as review
			// feedback and in the PR's thread comments.
			fake.mu.Lock()
			defer fake.mu.Unlock()
			humanReplies++
			fake.threads[5].replies = append(fake.threads[5].replies, map[string]any{
				"id": 100 + humanReplies, "parentCommentId": 1, "content": body, "commentType": "text",
				"author": map[string]string{"id": "human-guid", "displayName": "Human"},
			})
		},
		editThread: func(body string) { fake.mu.Lock(); fake.threads[5].root = body; fake.mu.Unlock() },
		replies: func() int {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return len(fake.threads[5].replies) - humanReplies
		},
		resolved: func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return fake.threads[5].status == "fixed" },
	}
}

// gatherIntoJournal runs gather-review-threads and records its result the
// way the executor does, so every later stage reads the snapshot it pinned.
func gatherIntoJournal(t *testing.T, w feedbackWorld, run *revisionRun) apiv1.RemediationBrief {
	t.Helper()
	resultFile := filepath.Join(t.TempDir(), remediationBriefResultFile)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	if code, stdout, stderr := runArgs(t, "gather-review-threads", w.root); code != 0 {
		t.Fatalf("gather-review-threads: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.run.RecordArtifact(w.runID+":gather-review-threads/result", data); err != nil {
		t.Fatal(err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		t.Fatal(err)
	}
	if brief.FeedbackSnapshot == nil {
		t.Fatalf("gather-review-threads recorded no feedback snapshot: %s", data)
	}
	return brief
}

func verifyFeedback(t *testing.T, w feedbackWorld) prRemediationLifecycleResult {
	t.Helper()
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), prRemediationLifecycleResultFile))
	code, stdout, stderr := runArgs(t, "pr-claim", "--verify-feedback", w.root)
	if code != 0 {
		t.Fatalf("pr-claim --verify-feedback: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	return readLifecycleResult(t)
}

// TestVerifyFeedbackDetectsChangesAcrossProviders is #6126 end to end on
// every provider: the snapshot gather-review-threads pinned is current until
// a human comments after it (new_feedback) or edits the thread with the head
// unchanged (changed_feedback). A stale verdict keeps the claim — it is a
// repass, not a terminal.
func TestVerifyFeedbackDetectsChangesAcrossProviders(t *testing.T) {
	for _, kind := range revisionProviders {
		for _, tc := range []struct {
			name   string
			change func(w feedbackWorld)
			want   string
		}{
			{name: "unchanged", change: func(feedbackWorld) {}, want: ""},
			{name: "new comment after snapshot", change: func(w feedbackWorld) { w.addComment("Also handle the nil case.") }, want: staleReasonNew},
			{name: "edited thread with the same head", change: func(w feedbackWorld) { w.editThread("Guard this write and log it.") }, want: staleReasonChanged},
		} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				w := newFeedbackWorld(t, kind)
				run := newRevisionRun(t, w.root, w.runID)
				run.selectHead("77", revisionSelectedSHA)
				brief := gatherIntoJournal(t, w, run)
				tc.change(w)
				result := verifyFeedback(t, w)
				if result.StaleInput != tc.want || !result.Open || result.NoWork {
					t.Fatalf("result = %+v, want staleInput %q with the claim kept", result, tc.want)
				}
				if result.FeedbackSnapshotDigest != brief.FeedbackSnapshot.SnapshotDigest {
					t.Fatalf("digest = %q, want the gathered snapshot's %q", result.FeedbackSnapshotDigest, brief.FeedbackSnapshot.SnapshotDigest)
				}
				if !prClaimHeld(t, w.root) {
					t.Fatal("a feedback check released the claim")
				}
			})
		}
	}
}

// TestGatherReviewThreadsRefusesMovedHeadAcrossProviders: feedback captured
// on a head the run never selected would be feedback about someone else's
// code, so the gather ends as a stale selection instead of pinning it.
func TestGatherReviewThreadsRefusesMovedHeadAcrossProviders(t *testing.T) {
	for _, kind := range revisionProviders {
		t.Run(string(kind), func(t *testing.T) {
			w := newFeedbackWorld(t, kind)
			newRevisionRun(t, w.root, w.runID).selectHead("77", revisionSelectedSHA)
			w.setHead(revisionMovedSHA)
			resultFile := filepath.Join(t.TempDir(), remediationBriefResultFile)
			t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
			code, stdout, stderr := runArgs(t, "gather-review-threads", w.root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			result := readJSONResult(t, resultFile)
			if result["noWork"] != true || result["outcome"] != prClaimOutcomeStaleSelection {
				t.Fatalf("result = %v, want a stale-selection no-work instead of a brief", result)
			}
			if prClaimHeld(t, w.root) {
				t.Fatal("stale selection kept the claim")
			}
		})
	}
}

// TestVerifyFeedbackToleratesPreSnapshotRun: a run whose brief was gathered
// by a binary without feedback snapshots (a v3 brief, no feedbackSnapshot)
// resumes under this one. There is nothing to compare, so the check passes
// rather than blocking or failing the in-flight run.
func TestVerifyFeedbackToleratesPreSnapshotRun(t *testing.T) {
	w := newFeedbackWorld(t, providers.ProviderGitHub)
	run := newRevisionRun(t, w.root, w.runID)
	v3 := map[string]any{
		"schema": "goobers.dev/remediation-brief/v3", "integrity": "unapproved", "selectedNumber": "77",
		"head": "work", "base": "main", "workspaceBranch": "work", "isBehindBase": false,
		"hasSubstantiveFindings": "true", "hasFailingCI": "false",
		"gatherPrContext":     map[string]any{"headSha": revisionSelectedSHA, "baseSha": "base-sha", "verdict": nil, "comments": []any{}},
		"gatherReviewThreads": map[string]any{"reviews": []any{}, "inlineComments": []any{}},
	}
	run.selectBrief(v3)
	w.addComment("A comment the old run never pinned.")
	result := verifyFeedback(t, w)
	if result.StaleInput != "" || result.FeedbackCheck != prFeedbackCheckUnrecorded || !result.Open {
		t.Fatalf("result = %+v, want an unrecorded, passing feedback check", result)
	}
}

// seedResolutionAfterGather appends what the agentic chain and publication
// leave in the journal after gather: implement's threadResponses and a
// published push at head published.
func seedResolutionAfterGather(t *testing.T, run *revisionRun, threadID, published string) {
	t.Helper()
	responses := `[{"threadId":"` + threadID + `","disposition":"addressed","detail":"guarded the write"}]`
	for _, event := range []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess), Outputs: map[string]any{threadResponsesOutput: responses}},
	} {
		if err := run.run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	run.publish(published)
}

func resolveThreads(t *testing.T, w feedbackWorld) (int, map[string]any) {
	t.Helper()
	resultFile := filepath.Join(t.TempDir(), resolveReviewThreadsResultFile)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	code, stdout, stderr := runArgs(t, "resolve-review-threads", w.root)
	if code != 0 {
		t.Logf("resolve-review-threads: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if _, err := os.Stat(resultFile); err != nil {
		return code, nil
	}
	return code, readJSONResult(t, resultFile)
}

var mutatingFeedbackProviders = []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO}

// TestResolveReviewThreadsRejectsStaleFeedbackAcrossProviders: after the
// branch is published, feedback that changed since it was gathered — a new
// reply, or the thread edited with the head unchanged — publishes no reply
// and resolves nothing, and reports the typed reason the workflow routes back
// to re-gather. Gitea is excluded: it cannot reply to or resolve threads.
func TestResolveReviewThreadsRejectsStaleFeedbackAcrossProviders(t *testing.T) {
	for _, kind := range mutatingFeedbackProviders {
		for _, tc := range []struct {
			name   string
			change func(w feedbackWorld)
			want   string
		}{
			{name: "new comment after snapshot", change: func(w feedbackWorld) { w.addComment("Also handle nil.") }, want: staleReasonNew},
			{name: "edited thread with the same head", change: func(w feedbackWorld) { w.editThread("Guard it and log it.") }, want: staleReasonChanged},
		} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				w := newFeedbackWorld(t, kind)
				setDaemonStageAttributionEnv(t)
				run := newRevisionRun(t, w.root, w.runID)
				run.selectHead("77", revisionSelectedSHA)
				gatherIntoJournal(t, w, run)
				seedResolutionAfterGather(t, run, w.threadID, revisionPublishedSHA)
				w.setHead(revisionPublishedSHA)
				tc.change(w)

				code, result := resolveThreads(t, w)
				if code != 0 || result[staleInputOutput] != tc.want {
					t.Fatalf("code = %d, result = %v, want staleInput %q", code, result, tc.want)
				}
				if w.replies() != 0 || w.resolved() {
					t.Fatalf("replies = %d resolved = %v, want nothing published against stale feedback", w.replies(), w.resolved())
				}
			})
		}
	}
}

// TestResolveReviewThreadsPublishesCurrentFeedbackAcrossProviders is the
// control: with nothing changed since gather, publication proceeds exactly
// as before — one verified reply, then the resolution — and a retry adds
// nothing.
func TestResolveReviewThreadsPublishesCurrentFeedbackAcrossProviders(t *testing.T) {
	for _, kind := range mutatingFeedbackProviders {
		t.Run(string(kind), func(t *testing.T) {
			w := newFeedbackWorld(t, kind)
			setDaemonStageAttributionEnv(t)
			run := newRevisionRun(t, w.root, w.runID)
			run.selectHead("77", revisionSelectedSHA)
			gatherIntoJournal(t, w, run)
			seedResolutionAfterGather(t, run, w.threadID, revisionPublishedSHA)
			w.setHead(revisionPublishedSHA)
			for attempt := 1; attempt <= 2; attempt++ {
				code, result := resolveThreads(t, w)
				if code != 0 || result[staleInputOutput] != "" || result[unresolvedReviewThreadCountOutput] != "0" {
					t.Fatalf("attempt %d: code = %d, result = %v", attempt, code, result)
				}
			}
			if w.replies() != 1 || !w.resolved() {
				t.Fatalf("replies = %d resolved = %v, want one reply and the thread resolved", w.replies(), w.resolved())
			}
		})
	}
}

// TestResolveReviewThreadsRetriesAnUnpropagatedPush: a provider still
// reporting the head this pass published over has not caught up with the
// push (Azure DevOps updates asynchronously). That is a retryable
// infrastructure condition, not a moved PR.
func TestResolveReviewThreadsRetriesAnUnpropagatedPush(t *testing.T) {
	w := newFeedbackWorld(t, providers.ProviderADO)
	run := newRevisionRun(t, w.root, w.runID)
	run.selectHead("77", revisionSelectedSHA)
	gatherIntoJournal(t, w, run)
	seedResolutionAfterGather(t, run, w.threadID, revisionPublishedSHA)
	code, result := resolveThreads(t, w)
	if code != 1 || result[executor.OutputErrorCode] != errorCodePublishedHeadNotVisible || result[executor.OutputErrorRetryable] != true {
		t.Fatalf("code = %d, result = %v, want a retryable %s", code, result, errorCodePublishedHeadNotVisible)
	}
	if w.replies() != 0 {
		t.Fatal("replied before the published head was visible")
	}
}

func TestResolveReviewThreadsEndsOnForeignHeadAcrossProviders(t *testing.T) {
	for _, kind := range mutatingFeedbackProviders {
		t.Run(string(kind), func(t *testing.T) {
			w := newFeedbackWorld(t, kind)
			run := newRevisionRun(t, w.root, w.runID)
			run.selectHead("77", revisionSelectedSHA)
			gatherIntoJournal(t, w, run)
			seedResolutionAfterGather(t, run, w.threadID, revisionPublishedSHA)
			w.setHead(revisionMovedSHA)
			code, result := resolveThreads(t, w)
			if code != 0 || result["noWork"] != true || result[staleInputOutput] != staleReasonHead {
				t.Fatalf("code = %d, result = %v, want a stale-head no-work", code, result)
			}
			if w.replies() != 0 || prClaimHeld(t, w.root) {
				t.Fatalf("replies = %d claimHeld = %v, want nothing published and the claim released", w.replies(), prClaimHeld(t, w.root))
			}
		})
	}
}
