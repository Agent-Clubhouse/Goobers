package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const adoReviewThreadsBase = "/org/project/_apis/git/repositories/repo/pullrequests/42"

type adoThreadFixture = map[string]interface{}

func adoThreadComment(id, parent int, authorID, content, commentType string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "parentCommentId": parent, "content": content, "commentType": commentType,
		"author":        map[string]string{"id": authorID, "displayName": "Name " + authorID},
		"publishedDate": "2026-09-01T10:00:00Z",
	}
}

func deletedADOThreadComment(comment map[string]interface{}) map[string]interface{} {
	comment["isDeleted"] = true
	return comment
}

func adoFileThread(id int, status, path string, line, iteration int, comments ...map[string]interface{}) adoThreadFixture {
	thread := adoThreadFixture{
		"id": id, "status": status, "comments": comments,
		"threadContext": map[string]interface{}{"filePath": path, "rightFileStart": map[string]int{"line": line, "offset": 1}},
	}
	if iteration > 0 {
		thread["pullRequestThreadContext"] = map[string]interface{}{
			"iterationContext": map[string]int{"firstComparingIteration": 1, "secondComparingIteration": iteration},
		}
	}
	return thread
}

// newADOReviewThreadsServer serves two pages of threads (continuation token),
// the identity, and the latest iteration's changes. iterationsStatus lets a
// test make the iteration read fail.
func newADOReviewThreadsServer(t *testing.T, iterationsStatus int) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"authenticatedUser": map[string]interface{}{"id": "SELF-GUID", "providerDisplayName": "Goobers"}})
	})
	mux.HandleFunc(adoReviewThreadsBase+"/threads", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		calls++
		switch r.URL.Query().Get("continuationToken") {
		case "":
			w.Header().Set("x-ms-continuationtoken", "page-2")
			writeJSON(t, w, map[string]interface{}{"value": []adoThreadFixture{
				// Live: written on the latest iteration, with a Goobers reply.
				adoFileThread(1, "active", "/src/a.go", 10, 2,
					adoThreadComment(1, 0, "reviewer-guid", "please fix", "text"),
					adoThreadComment(2, 1, "self-guid", "done", "text"),
					deletedADOThreadComment(adoThreadComment(3, 1, "reviewer-guid", "gone", "text"))),
				// ADO-synthesized vote thread.
				{"id": 2, "comments": []map[string]interface{}{adoThreadComment(1, 0, "reviewer-guid", "voted", "system")}},
				// Deleted thread.
				{"id": 3, "status": "active", "isDeleted": true, "comments": []map[string]interface{}{adoThreadComment(1, 0, "reviewer-guid", "x", "text")}},
				// Goobers' own thread (identity matched case-insensitively by id).
				{"id": 4, "status": "closed", "comments": []map[string]interface{}{adoThreadComment(1, 0, "self-guid", "verdict", "text")}},
			}})
		case "page-2":
			writeJSON(t, w, map[string]interface{}{"value": []adoThreadFixture{
				// Older iteration, file still in the diff: live.
				adoFileThread(5, "pending", "/src/a.go", 20, 1, adoThreadComment(1, 0, "reviewer-guid", "older", "text")),
				// Older iteration, file no longer in the diff: outdated.
				adoFileThread(6, "fixed", "/src/removed.go", 3, 1, adoThreadComment(1, 0, "reviewer-guid", "stale", "text")),
				// No iteration context, file not in the diff: live (fails open).
				adoFileThread(7, "active", "/src/removed.go", 4, 0, adoThreadComment(1, 0, "reviewer-guid", "no ctx", "text")),
				// General comment, no file anchor: skipped (not a review thread).
				{"id": 8, "status": "active", "comments": []map[string]interface{}{adoThreadComment(1, 0, "reviewer-guid", "general", "text")}},
				// A context with an empty filePath is general too: skipped.
				{"id": 11, "status": "active", "comments": []map[string]interface{}{adoThreadComment(1, 0, "reviewer-guid", "general", "text")},
					"threadContext": map[string]interface{}{"filePath": ""}},
				// Unknown status is unresolved.
				adoFileThread(10, "unknown", "/src/a.go", 30, 0, adoThreadComment(1, 0, "reviewer-guid", "odd status", "text")),
				// Left-side anchor.
				{"id": 9, "status": "wontFix", "comments": []map[string]interface{}{adoThreadComment(1, 0, "reviewer-guid", "left", "text")},
					"threadContext": map[string]interface{}{"filePath": "/src/a.go", "leftFileStart": map[string]int{"line": 7}}},
			}})
		default:
			t.Fatalf("unexpected continuation token %q", r.URL.Query().Get("continuationToken"))
		}
	})
	mux.HandleFunc(adoReviewThreadsBase+"/iterations", func(w http.ResponseWriter, _ *http.Request) {
		if iterationsStatus != http.StatusOK {
			w.WriteHeader(iterationsStatus)
			return
		}
		writeJSON(t, w, map[string]interface{}{"value": []map[string]int{{"id": 2}, {"id": 1}}})
	})
	mux.HandleFunc(adoReviewThreadsBase+"/iterations/2/changes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"changeEntries": []map[string]interface{}{
			{"changeType": "edit", "item": map[string]string{"path": "/src/a.go"}},
		}})
	})
	return httptest.NewServer(mux), &calls
}

func newADOReviewThreadsProvider(serverURL string) *ADOProvider {
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = serverURL })
}

func TestADOListPullRequestReviewThreadsMapsAndSkips(t *testing.T) {
	server, calls := newADOReviewThreadsServer(t, http.StatusOK)
	defer server.Close()

	got, err := newADOReviewThreadsProvider(server.URL).ListPullRequestReviewThreads(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "42")
	if err != nil {
		t.Fatalf("ListPullRequestReviewThreads: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("thread pages read = %d, want 2 (continuation token followed)", *calls)
	}
	if got.Reviews == nil || len(got.Reviews) != 0 {
		t.Fatalf("Reviews = %#v, want empty non-nil (ADO votes carry no review body)", got.Reviews)
	}
	type key struct {
		thread string
		id     int64
	}
	byKey := map[key]PullRequestInlineComment{}
	for _, c := range got.InlineComments {
		byKey[key{c.ThreadID, c.ID}] = c
	}
	wantThreads := map[string]bool{"42/1": true, "42/5": true, "42/6": true, "42/7": true, "42/9": true, "42/10": true}
	for _, c := range got.InlineComments {
		if !wantThreads[c.ThreadID] {
			t.Errorf("unexpected thread %q in output (system, deleted, general and own threads must be skipped)", c.ThreadID)
		}
	}
	if len(got.InlineComments) != 7 {
		t.Fatalf("inline comments = %d, want 7 (the deleted reply dropped): %#v", len(got.InlineComments), got.InlineComments)
	}

	root := byKey[key{"42/1", 1}]
	if root.Path != "src/a.go" || root.Line != 10 || root.Side != "RIGHT" || root.IsResolved || root.IsOutdated || root.InReplyTo != 0 {
		t.Errorf("root comment = %#v", root)
	}
	if root.Author != "Name reviewer-guid" || root.CreatedAt == nil || root.Body != "please fix" {
		t.Errorf("root comment author/body/time = %#v", root)
	}
	if !strings.HasSuffix(root.URL, "/org/project/_git/repo/pullrequest/42?discussionId=1") {
		t.Errorf("root URL = %q", root.URL)
	}
	if reply := byKey[key{"42/1", 2}]; reply.InReplyTo != 1 || reply.Body != "done" {
		t.Errorf("reply inside a reviewer thread must be kept with InReplyTo = parentCommentId: %#v", reply)
	}
	if byKey[key{"42/5", 1}].IsOutdated || byKey[key{"42/5", 1}].IsResolved {
		t.Errorf("pending thread on a file still in the diff must be live and unresolved: %#v", byKey[key{"42/5", 1}])
	}
	if c := byKey[key{"42/6", 1}]; !c.IsOutdated || !c.IsResolved {
		t.Errorf("fixed thread on a file no longer in the diff must be resolved and outdated: %#v", c)
	}
	if c := byKey[key{"42/7", 1}]; c.IsOutdated {
		t.Errorf("thread without iteration context must be live: %#v", c)
	}
	if c := byKey[key{"42/10", 1}]; c.Path != "src/a.go" || c.Line != 30 || c.IsResolved {
		t.Errorf("unknown status must be unresolved: %#v", c)
	}
	if c := byKey[key{"42/9", 1}]; c.Side != "LEFT" || c.Line != 7 || !c.IsResolved {
		t.Errorf("left-anchored wontFix thread = %#v", c)
	}
}

func TestADOListPullRequestReviewThreadsOutdatedFailsOpen(t *testing.T) {
	server, _ := newADOReviewThreadsServer(t, http.StatusNotFound)
	defer server.Close()

	got, err := newADOReviewThreadsProvider(server.URL).ListPullRequestReviewThreads(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "42")
	if err != nil {
		t.Fatalf("ListPullRequestReviewThreads: %v", err)
	}
	for _, c := range got.InlineComments {
		if c.IsOutdated {
			t.Errorf("thread %s is outdated although the iteration read failed; the rule must fail open", c.ThreadID)
		}
	}
}

func TestADOReviewThreadStatusMapping(t *testing.T) {
	for status, want := range map[string]bool{
		"active": false, "pending": false, "unknown": false, "": false,
		"fixed": true, "wontFix": true, "closed": true, "byDesign": true,
	} {
		if got := adoReviewThreadIsResolved(status); got != want {
			t.Errorf("adoReviewThreadIsResolved(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestADOReplyPullRequestReviewThread(t *testing.T) {
	var posted map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc(adoReviewThreadsBase+"/threads/5/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		decodeJSON(t, r, &posted)
		writeJSON(t, w, adoThreadComment(3, 1, "self-guid", "addressed", "text"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	recorder := &adoMutationRecorder{}
	provider := newADOReviewThreadsProvider(server.URL)
	provider.SetMutationRecorder(recorder)
	repo := RepositoryRef{Name: "repo", Project: "project"}

	reply, err := provider.ReplyPullRequestReviewThread(context.Background(), PullRequestReviewThreadReply{
		Repository: repo, PullID: "42", ThreadID: "42/5", CommentID: 1, Body: "addressed",
	})
	if err != nil {
		t.Fatalf("ReplyPullRequestReviewThread: %v", err)
	}
	if posted["content"] != "addressed" || posted["parentCommentId"] != float64(1) || posted["commentType"] != "text" {
		t.Fatalf("posted = %#v", posted)
	}
	if reply.ID != 3 || reply.ThreadID != "42/5" || reply.InReplyTo != 1 || reply.Body != "addressed" {
		t.Fatalf("reply = %#v", reply)
	}
	if len(recorder.refs) != 1 || recorder.refs[0].Operation != "review-thread-reply" {
		t.Fatalf("recorded refs = %#v", recorder.refs)
	}

	for name, req := range map[string]PullRequestReviewThreadReply{
		"missing thread id": {Repository: repo, PullID: "42", CommentID: 1, Body: "x"},
		"other pull":        {Repository: repo, PullID: "42", ThreadID: "41/5", CommentID: 1, Body: "x"},
		"missing comment":   {Repository: repo, PullID: "42", ThreadID: "42/5", Body: "x"},
	} {
		if _, err := provider.ReplyPullRequestReviewThread(context.Background(), req); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestADOResolvePullRequestReviewThread(t *testing.T) {
	echo := "fixed"
	var patched map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc(adoReviewThreadsBase+"/threads/5", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		decodeJSON(t, r, &patched)
		writeJSON(t, w, map[string]interface{}{"id": 5, "status": echo})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := newADOReviewThreadsProvider(server.URL)
	repo := RepositoryRef{Name: "repo", Project: "project"}

	if err := provider.ResolvePullRequestReviewThread(context.Background(), repo, "42/5"); err != nil {
		t.Fatalf("ResolvePullRequestReviewThread: %v", err)
	}
	if len(patched) != 1 || patched["status"] != "fixed" {
		t.Fatalf("PATCH body = %#v, want exactly {status: fixed}", patched)
	}
	echo = "active"
	if err := provider.ResolvePullRequestReviewThread(context.Background(), repo, "42/5"); err == nil {
		t.Fatal("want an error when ADO does not echo the fixed status")
	}
	for _, bad := range []string{"", "42", "42/x", "/5", "42/5/1"} {
		if err := provider.ResolvePullRequestReviewThread(context.Background(), repo, bad); err == nil {
			t.Errorf("thread id %q: want error", bad)
		}
	}
}
