package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func adoFeedbackComment(id, parent int, authorID, content, published string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "parentCommentId": parent, "content": content, "commentType": "text",
		"author":        map[string]string{"id": authorID, "displayName": "Same Name"},
		"publishedDate": published,
	}
}

func newADOFeedbackProvider(t *testing.T, pages ...[]adoThreadFixture) *ADOProvider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(adoReviewThreadsBase+"/threads", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		page := 0
		if token := r.URL.Query().Get("continuationToken"); token != "" {
			page, _ = strconv.Atoi(strings.TrimPrefix(token, "page-"))
		}
		if page+1 < len(pages) {
			w.Header().Set("x-ms-continuationtoken", "page-"+strconv.Itoa(page+1))
		}
		writeJSON(t, w, map[string]interface{}{"value": pages[page]})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
}

func adoFeedbackRepo() RepositoryRef {
	return RepositoryRef{Provider: ProviderADO, Owner: "org", Project: "project", Name: "repo"}
}

// TestADOFeedbackCommentsSkipSystemAndDeleted: a synthesized thread (a
// CodeReviewThreadType property), a system comment, a deleted comment and a
// deleted thread never reach the watermark, while a general comment and a
// file-thread reply do.
func TestADOFeedbackCommentsSkipSystemAndDeleted(t *testing.T) {
	provider := newADOFeedbackProvider(t, []adoThreadFixture{
		{"id": 1, "comments": []map[string]interface{}{adoFeedbackComment(1, 0, "human-guid", "general", "2026-09-01T10:00:00Z")}},
		{"id": 2, "properties": map[string]interface{}{"CodeReviewThreadType": map[string]string{"$type": "System.String", "$value": "VoteUpdate"}},
			"comments": []map[string]interface{}{adoFeedbackComment(1, 0, "human-guid", "voted", "2026-09-01T11:00:00Z")}},
		{"id": 3, "comments": []map[string]interface{}{func() map[string]interface{} {
			c := adoFeedbackComment(1, 0, "system-guid", "pushed", "2026-09-01T11:00:00Z")
			c["commentType"] = "system"
			return c
		}()}},
		adoFileThread(4, "active", "/a.go", 3, 0,
			adoFeedbackComment(1, 0, "human-guid", "root", "2026-09-01T09:00:00Z"),
			adoFeedbackComment(2, 1, "human-guid", "reply", "2026-09-01T12:00:00Z"),
			deletedADOThreadComment(adoFeedbackComment(3, 1, "human-guid", "", "2026-09-01T13:00:00Z"))),
		{"id": 5, "isDeleted": true, "comments": []map[string]interface{}{adoFeedbackComment(1, 0, "human-guid", "gone", "2026-09-01T14:00:00Z")}},
	})
	got, err := provider.ListPullRequestFeedbackComments(context.Background(), adoFeedbackRepo(), "42")
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, c := range got {
		bodies = append(bodies, c.Body)
		if c.AuthorID != "human-guid" {
			t.Errorf("comment %q AuthorID = %q, want human-guid", c.Body, c.AuthorID)
		}
	}
	if strings.Join(bodies, ",") != "root,general,reply" {
		t.Fatalf("feedback bodies = %v, want [root general reply] in publish order", bodies)
	}
}

// TestADOFeedbackCommentsOrderTiesByThreadThenComment: equal server times are
// ordered by thread id, then comment id, whatever order ADO returned them in.
func TestADOFeedbackCommentsOrderTiesByThreadThenComment(t *testing.T) {
	const at = "2026-09-01T10:00:00Z"
	provider := newADOFeedbackProvider(t, []adoThreadFixture{
		{"id": 9, "comments": []map[string]interface{}{adoFeedbackComment(2, 0, "a", "9/2", at), adoFeedbackComment(1, 0, "a", "9/1", at)}},
		{"id": 3, "comments": []map[string]interface{}{adoFeedbackComment(1, 0, "a", "3/1", at)}},
	})
	got, err := provider.ListPullRequestFeedbackComments(context.Background(), adoFeedbackRepo(), "42")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.Body)
	}
	if strings.Join(ids, ",") != "3/1,9/1,9/2" {
		t.Fatalf("order = %v, want [3/1 9/1 9/2]", ids)
	}
}

// TestADOFeedbackCommentsFailClosed: an incomplete identity, an unreadable
// timestamp and a thread read past its page bound are errors, never a partial
// comment set.
func TestADOFeedbackCommentsFailClosed(t *testing.T) {
	t.Run("missing author id", func(t *testing.T) {
		provider := newADOFeedbackProvider(t, []adoThreadFixture{
			{"id": 1, "comments": []map[string]interface{}{adoFeedbackComment(1, 0, "", "who", "2026-09-01T10:00:00Z")}},
		})
		if _, err := provider.ListPullRequestFeedbackComments(context.Background(), adoFeedbackRepo(), "42"); err == nil || !strings.Contains(err.Error(), "no author id") {
			t.Fatalf("err = %v, want a missing-author-id error", err)
		}
	})
	t.Run("unreadable publishedDate", func(t *testing.T) {
		provider := newADOFeedbackProvider(t, []adoThreadFixture{
			{"id": 1, "comments": []map[string]interface{}{adoFeedbackComment(1, 0, "a", "when", "yesterday")}},
		})
		if _, err := provider.ListPullRequestFeedbackComments(context.Background(), adoFeedbackRepo(), "42"); err == nil || !strings.Contains(err.Error(), "publishedDate") {
			t.Fatalf("err = %v, want a publishedDate error", err)
		}
	})
	t.Run("page bound", func(t *testing.T) {
		pages := make([][]adoThreadFixture, ADOFeedbackThreadPageLimit+1)
		for i := range pages {
			pages[i] = []adoThreadFixture{{"id": i + 1, "comments": []map[string]interface{}{adoFeedbackComment(1, 0, "a", "x", "2026-09-01T10:00:00Z")}}}
		}
		provider := newADOFeedbackProvider(t, pages...)
		if _, err := provider.ListPullRequestFeedbackComments(context.Background(), adoFeedbackRepo(), "42"); err == nil || !strings.Contains(err.Error(), "refusing a partial thread set") {
			t.Fatalf("err = %v, want the page-bound refusal", err)
		}
	})
	t.Run("pages within the bound are all read", func(t *testing.T) {
		pages := make([][]adoThreadFixture, ADOFeedbackThreadPageLimit)
		for i := range pages {
			pages[i] = []adoThreadFixture{{"id": i + 1, "comments": []map[string]interface{}{adoFeedbackComment(1, 0, "a", "x", "2026-09-01T10:00:00Z")}}}
		}
		provider := newADOFeedbackProvider(t, pages...)
		got, err := provider.ListPullRequestFeedbackComments(context.Background(), adoFeedbackRepo(), "42")
		if err != nil || len(got) != ADOFeedbackThreadPageLimit {
			t.Fatalf("got %d comments, err %v; want %d", len(got), err, ADOFeedbackThreadPageLimit)
		}
	})
}
