package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// ADO-N20: gather-review-threads and resolve-review-threads run on Azure
// DevOps through the narrow review-thread surfaces, against a fake ADO server.

// adoReviewThread is one fake ADO pull-request thread: a reviewer's root
// comment plus the replies posted to it.
type adoReviewThread struct {
	status  string
	path    string
	replies []map[string]any
	// root overrides the reviewer's root comment text ("finding <id>").
	root string
}

type fakeADOReviewThreads struct {
	t        *testing.T
	mu       sync.Mutex
	threads  map[int]*adoReviewThread
	order    []string
	resolved []int
	// head, when set, overrides the served source head so a test can move
	// the PR between stages.
	head string
	// fault is a one-shot injected failure at a mutation boundary (#6131):
	// "reply"/"resolve" fail unapplied, "reply-applied"/"resolve-applied"
	// apply and then fail, the way a lost response does.
	fault string
}

// takeFault consumes the armed fault when it is one of kinds.
func (f *fakeADOReviewThreads) takeFault(kinds ...string) string {
	for _, kind := range kinds {
		if f.fault == kind {
			f.fault = ""
			return kind
		}
	}
	return ""
}

func writeInjectedADOFault(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"message":"injected fault"}`))
}

func (f *fakeADOReviewThreads) threadsJSON() map[string]any {
	ids := make([]int, 0, len(f.threads))
	for id := range f.threads {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	value := []map[string]any{
		// Goobers' own verdict thread: skipped by author id.
		{"id": 1, "status": "closed", "comments": []map[string]any{{
			"id": 1, "parentCommentId": 0, "content": stampOwnFixtureBody("verdict", "comment"), "commentType": "text",
			"author": map[string]string{"id": "self-guid", "displayName": "Goobers"},
		}}},
		// ADO-synthesized vote thread: skipped as system.
		{"id": 2, "comments": []map[string]any{{"id": 1, "commentType": "system", "content": "voted"}}},
		// A reviewer's general PR comment with no file anchor: a conversation
		// comment, not a review thread, so it is skipped. Were it emitted, its
		// empty path would fail the remediation brief schema.
		{"id": 3, "status": "active", "comments": []map[string]any{{
			"id": 1, "parentCommentId": 0, "content": "general remark", "commentType": "text",
			"author":        map[string]string{"id": "reviewer-guid", "displayName": "Reviewer"},
			"publishedDate": "2026-09-01T10:00:00Z",
		}}},
	}
	for _, id := range ids {
		thread := f.threads[id]
		root := thread.root
		if root == "" {
			root = "finding " + strconv.Itoa(id)
		}
		comments := []map[string]any{{
			"id": 1, "parentCommentId": 0, "content": root, "commentType": "text",
			"author":        map[string]string{"id": "reviewer-guid", "displayName": "Reviewer"},
			"publishedDate": "2026-09-01T10:00:00Z",
		}}
		comments = append(comments, thread.replies...)
		value = append(value, map[string]any{
			"id": id, "status": thread.status, "comments": comments,
			"threadContext": map[string]any{"filePath": "/" + thread.path, "rightFileStart": map[string]int{"line": id}},
		})
	}
	return map[string]any{"value": value}
}

func (f *fakeADOReviewThreads) server(repo providers.RepositoryRef, publishedHead string) *httptest.Server {
	t := f.t
	pr := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests/77"
	mux := http.NewServeMux()
	mux.HandleFunc("/"+repo.Owner+"/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]any{"authenticatedUser": map[string]any{"id": "self-guid", "providerDisplayName": "Goobers"}})
	})
	mux.HandleFunc(pr, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		publishedHead := publishedHead
		if f.head != "" {
			publishedHead = f.head
		}
		f.mu.Unlock()
		writeJSONResp(t, w, map[string]any{
			"pullRequestId": 77, "status": "active", "title": "t",
			"sourceRefName": "refs/heads/goobers/work", "targetRefName": "refs/heads/main",
			"lastMergeSourceCommit": map[string]string{"commitId": publishedHead},
			"lastMergeTargetCommit": map[string]string{"commitId": "base-sha"},
			"createdBy":             map[string]string{"displayName": "goober"},
			"repository": map[string]any{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]any{"value": []any{}})
	})
	mux.HandleFunc(pr+"/threads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("threads method = %s, want GET (review-thread stages never open threads)", r.Method)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSONResp(t, w, f.threadsJSON())
	})
	mux.HandleFunc(pr+"/threads/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.Split(strings.TrimPrefix(r.URL.Path, pr+"/threads/"), "/")
		id, _ := strconv.Atoi(rest[0])
		thread := f.threads[id]
		if thread == nil {
			t.Errorf("request to unknown thread %d: %s %s", id, r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == http.MethodPost && len(rest) == 2 && rest[1] == "comments":
			if body["parentCommentId"] != float64(1) {
				t.Errorf("reply parentCommentId = %v, want the root comment 1", body["parentCommentId"])
			}
			fault := f.takeFault("reply", "reply-applied")
			if fault == "reply" {
				writeInjectedADOFault(w)
				return
			}
			reply := map[string]any{
				"id": len(thread.replies) + 2, "parentCommentId": 1, "content": body["content"], "commentType": "text",
				"author": map[string]string{"id": "self-guid", "displayName": "Goobers"},
			}
			thread.replies = append(thread.replies, reply)
			f.order = append(f.order, "reply:"+rest[0])
			if fault != "" {
				writeInjectedADOFault(w)
				return
			}
			writeJSONResp(t, w, reply)
		case r.Method == http.MethodPatch && len(rest) == 1:
			if len(thread.replies) == 0 {
				t.Errorf("thread %d resolved before its reply was visible", id)
			}
			if len(body) != 1 || body["status"] != "fixed" {
				t.Errorf("PATCH body = %v, want {status: fixed}", body)
			}
			fault := f.takeFault("resolve", "resolve-applied")
			if fault == "resolve" {
				writeInjectedADOFault(w)
				return
			}
			thread.status = "fixed"
			f.resolved = append(f.resolved, id)
			f.order = append(f.order, "resolve:"+rest[0])
			if fault != "" {
				writeInjectedADOFault(w)
				return
			}
			writeJSONResp(t, w, map[string]any{"id": id, "status": thread.status})
		default:
			t.Errorf("unexpected thread request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

// adoReviewThreadsStageFixture routes the stage to an ADO repository and
// delivers a distinct value for every credentialed capability: on ADO the
// review-thread stages consume the declared github:pr:write credential like
// every other stage (ADO-N18).
func adoReviewThreadsStageFixture(t *testing.T, runID string) (string, providers.RepositoryRef) {
	t.Helper()
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	deliverEveryADOStageCapability(t)
	return root, repo
}

func routeADOStageProvider(t *testing.T, serverURL string) {
	t.Helper()
	original := newADOProviderForStage
	newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		if routed.Provider != providers.ProviderADO {
			t.Fatalf("provider = %q, want ado", routed.Provider)
		}
		if got := deliveredCapabilityOf(t, credential); got != string(capability.GitHubPRWrite) {
			t.Fatalf("review-thread stage built its ADO provider from %q, want the declared %q", got, capability.GitHubPRWrite)
		}
		provider, err := buildADOProviderForStage(routed, credential)
		if err != nil {
			return nil, err
		}
		provider.BaseURL = serverURL
		return provider, nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })
}

func TestGatherReviewThreadsOnADO(t *testing.T) {
	const runID = "ado-gather-threads"
	root, repo := adoReviewThreadsStageFixture(t, runID)
	seedReviewThreadsBrief(t, root, runID, reviewThreadsBrief())
	fake := &fakeADOReviewThreads{t: t, threads: map[int]*adoReviewThread{
		5: {status: "active", path: "worker.go"},
		6: {status: "fixed", path: "old.go"},
	}}
	server := fake.server(repo, revisionSelectedSHA)
	defer server.Close()
	routeADOStageProvider(t, server.URL)
	dir := t.TempDir()
	t.Chdir(dir)

	if code, stdout, stderr := runArgs(t, "gather-review-threads", root); code != 0 {
		t.Fatalf("gather-review-threads: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, remediationBriefResultFile))
	if err != nil {
		t.Fatal(err)
	}
	var got apiv1.RemediationBrief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	section := got.GatherReviewThreads
	if section == nil || len(section.Reviews) != 0 || len(section.InlineComments) != 2 {
		t.Fatalf("gathered review threads = %#v, want 2 inline comments and no reviews (own, system and general threads skipped)", section)
	}
	live, resolved := section.InlineComments[0], section.InlineComments[1]
	if live.ThreadID != "77/5" || live.ID != 1 || live.Path != "worker.go" || live.Line != 5 || live.IsResolved || live.IsOutdated {
		t.Errorf("live comment = %#v", live)
	}
	if resolved.ThreadID != "77/6" || !resolved.IsResolved {
		t.Errorf("fixed thread comment = %#v, want resolved", resolved)
	}
}

func TestResolveReviewThreadsOnADO(t *testing.T) {
	const runID = "ado-resolve-threads"
	root, repo := adoReviewThreadsStageFixture(t, runID)
	seedReviewThreadResolutionRunWithComments(t, root, runID, `[
		{"threadId":"77/5","disposition":"addressed","detail":"added synchronization"},
		{"threadId":"77/6","disposition":"obsolete","detail":"code was removed"},
		{"threadId":"77/7","disposition":"blocked","detail":"needs maintainer input"}
	]`, []apiv1.RemediationInlineComment{
		{ID: 1, ThreadID: "77/5", Body: "fix", Path: "a.go", Integrity: apiv1.IntegrityUnapproved},
		{ID: 1, ThreadID: "77/6", Body: "old", Path: "b.go", Integrity: apiv1.IntegrityUnapproved},
		{ID: 1, ThreadID: "77/7", Body: "blocked", Path: "c.go", Integrity: apiv1.IntegrityUnapproved},
	})
	fake := &fakeADOReviewThreads{t: t, threads: map[int]*adoReviewThread{
		5: {status: "active", path: "a.go"},
		6: {status: "active", path: "b.go"},
		7: {status: "pending", path: "c.go"},
	}}
	server := fake.server(repo, "published-sha")
	defer server.Close()
	routeADOStageProvider(t, server.URL)
	setDaemonStageAttributionEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)

	// The second run is a stage retry: it must recognise the attributed
	// replies the first run posted and add none of its own.
	for attempt := 1; attempt <= 2; attempt++ {
		if code, stdout, stderr := runArgs(t, "resolve-review-threads", root); code != 0 {
			t.Fatalf("resolve-review-threads attempt %d: code = %d, stdout = %q, stderr = %q", attempt, code, stdout, stderr)
		}
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
	if len(fake.resolved) != 1 || fake.resolved[0] != 5 {
		t.Fatalf("resolved threads = %v, want only the addressed thread 5", fake.resolved)
	}
	for _, id := range []int{5, 6, 7} {
		if len(fake.threads[id].replies) != 1 {
			t.Errorf("thread %d replies = %d, want exactly 1", id, len(fake.threads[id].replies))
			continue
		}
		assertAttributedReviewThreadReply(t, strconv.Itoa(id), fake.threads[id].replies[0]["content"].(string))
	}
	if strings.Join(fake.order, ",") != "reply:5,resolve:5,reply:6,reply:7" {
		t.Errorf("mutation order = %v, want each reply before its resolution", fake.order)
	}
}
