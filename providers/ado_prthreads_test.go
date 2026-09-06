package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type adoMutationRecorder struct {
	refs []ExternalRef
}

func (r *adoMutationRecorder) RecordExternalRef(_ context.Context, ref ExternalRef) {
	r.refs = append(r.refs, ref)
}

func TestADOProviderPostPullRequestThreadComment(t *testing.T) {
	type threadComment struct {
		ParentCommentID int    `json:"parentCommentId"`
		Content         string `json:"content"`
		CommentType     string `json:"commentType"`
	}
	type threadPost struct {
		Comments []threadComment `json:"comments"`
		Status   string          `json:"status"`
	}
	var posted threadPost
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/threads", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		if got := r.URL.Query().Get("api-version"); got != "7.1" {
			t.Fatalf("api-version = %q, want 7.1", got)
		}
		decodeJSON(t, r, &posted)
		writeJSON(t, w, map[string]interface{}{
			"id":     7,
			"status": "active",
			"comments": []map[string]interface{}{{
				"id":              1,
				"parentCommentId": 0,
				"content":         "goobers merge-review verdict-json {...}",
				"commentType":     "text",
				"author":          map[string]string{"displayName": "Goobers Bot", "uniqueName": "bot@example.com", "id": "author-guid"},
				"publishedDate":   "2026-08-08T10:00:00Z",
			}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	recorder := &adoMutationRecorder{}
	provider := NewADOProvider("org", "project", "token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
	)
	provider.SetMutationRecorder(recorder)
	comment, err := provider.PostPullRequestThreadComment(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		"42",
		"goobers merge-review verdict-json {...}",
	)
	if err != nil {
		t.Fatalf("PostPullRequestThreadComment returned error: %v", err)
	}
	if len(posted.Comments) != 1 {
		t.Fatalf("posted comments = %#v", posted.Comments)
	}
	if posted.Comments[0].ParentCommentID != 0 ||
		posted.Comments[0].Content != "goobers merge-review verdict-json {...}" ||
		posted.Comments[0].CommentType != "text" {
		t.Fatalf("posted comment body = %#v", posted.Comments[0])
	}
	if posted.Status != "active" {
		t.Fatalf("posted status = %q, want active", posted.Status)
	}
	if comment.ID != "42/7/1" {
		t.Fatalf("comment.ID = %q, want 42/7/1", comment.ID)
	}
	if comment.Author != "Goobers Bot" {
		t.Fatalf("comment.Author = %q, want Goobers Bot", comment.Author)
	}
	if comment.Body != "goobers merge-review verdict-json {...}" {
		t.Fatalf("comment.Body = %q", comment.Body)
	}
	if len(recorder.refs) != 1 || recorder.refs[0].Provider != ProviderADO ||
		recorder.refs[0].Ref != "ado#42" || recorder.refs[0].Operation != "comment" {
		t.Fatalf("mutation refs = %#v", recorder.refs)
	}
	if comment.CreatedAt == nil || comment.CreatedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-08-08T10:00:00Z" {
		t.Fatalf("comment.CreatedAt = %v", comment.CreatedAt)
	}
}

func TestADOProviderListPullRequestThreadComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/threads", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
			{
				"id": 7,
				"comments": []map[string]interface{}{
					{"id": 1, "content": "verdict", "commentType": "text", "author": map[string]string{"displayName": "Reviewer"}, "publishedDate": "2026-08-08T10:00:00Z"},
					{"id": 2, "content": "finding history", "commentType": "text", "author": map[string]string{"displayName": "Reviewer"}, "publishedDate": "2026-08-08T10:05:00Z"},
				},
			},
			{
				// A system thread (vote/status/ref event) must be skipped.
				"id":       8,
				"comments": []map[string]interface{}{{"id": 3, "content": "Goobers Bot voted -10", "commentType": "system", "author": map[string]string{"displayName": "system"}}},
			},
			{
				"id":       9,
				"comments": []map[string]interface{}{{"id": 4, "content": "sticky remediation-state head=abc123", "commentType": "text", "author": map[string]string{"displayName": "Checkpoint"}}},
			},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	comments, err := provider.ListPullRequestThreadComments(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		"42",
	)
	if err != nil {
		t.Fatalf("ListPullRequestThreadComments returned error: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("len(comments) = %d, want 3 (system thread skipped): %#v", len(comments), comments)
	}
	wantIDs := []string{"42/7/1", "42/7/2", "42/9/4"}
	wantBodies := []string{"verdict", "finding history", "sticky remediation-state head=abc123"}
	wantAuthors := []string{"Reviewer", "Reviewer", "Checkpoint"}
	for i, c := range comments {
		if c.ID != wantIDs[i] {
			t.Fatalf("comments[%d].ID = %q, want %q", i, c.ID, wantIDs[i])
		}
		if c.Body != wantBodies[i] {
			t.Fatalf("comments[%d].Body = %q, want %q", i, c.Body, wantBodies[i])
		}
		if c.Author != wantAuthors[i] {
			t.Fatalf("comments[%d].Author = %q, want %q", i, c.Author, wantAuthors[i])
		}
	}
}

func TestADOProviderUpdatePullRequestThreadCommentRecordsMutation(t *testing.T) {
	type updateBody struct {
		Content string `json:"content"`
	}
	var patched updateBody
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/threads/7/comments/1", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		if got := r.URL.Query().Get("api-version"); got != "7.1" {
			t.Fatalf("api-version = %q, want 7.1", got)
		}
		decodeJSON(t, r, &patched)
		called = true
		writeJSON(t, w, map[string]interface{}{"id": 1, "content": patched.Content})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	recorder := &adoMutationRecorder{}
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	provider.SetMutationRecorder(recorder)
	repo := RepositoryRef{Name: "repo", Project: "project"}
	if err := provider.UpdatePullRequestThreadComment(context.Background(), repo, "42/7/1", "sticky remediation-state head=def456"); err != nil {
		t.Fatalf("UpdatePullRequestThreadComment returned error: %v", err)
	}
	if !called {
		t.Fatal("PATCH endpoint was not called")
	}
	if patched.Content != "sticky remediation-state head=def456" {
		t.Fatalf("patched content = %q", patched.Content)
	}
	if len(recorder.refs) != 1 || recorder.refs[0].Provider != ProviderADO ||
		recorder.refs[0].Ref != "ado#42" || recorder.refs[0].Operation != "comment" {
		t.Fatalf("mutation refs = %#v", recorder.refs)
	}

	// A malformed composite id must fail before any HTTP call.
	for _, bad := range []string{"", "1", "42/7", "42/x/1", "42/7/y"} {
		if err := provider.UpdatePullRequestThreadComment(context.Background(), repo, bad, "body"); err == nil {
			t.Fatalf("UpdatePullRequestThreadComment(%q) returned nil error, want parse failure", bad)
		}
	}
}

func TestADOProviderAddPullRequestLabels(t *testing.T) {
	var postedNames []string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/labels", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		if got := r.URL.Query().Get("api-version"); got != "7.1-preview.1" {
			t.Fatalf("api-version = %q, want 7.1-preview.1", got)
		}
		var body struct {
			Name string `json:"name"`
		}
		decodeJSON(t, r, &body)
		postedNames = append(postedNames, body.Name)
		writeJSON(t, w, map[string]interface{}{"id": "label-guid", "name": body.Name})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	err := provider.AddPullRequestLabels(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		"42",
		[]string{"goobers:needs-remediation", "  ", "goobers:merge-escalated"},
	)
	if err != nil {
		t.Fatalf("AddPullRequestLabels returned error: %v", err)
	}
	if len(postedNames) != 2 {
		t.Fatalf("posted names = %v, want 2 (blank skipped)", postedNames)
	}
	if postedNames[0] != "goobers:needs-remediation" || postedNames[1] != "goobers:merge-escalated" {
		t.Fatalf("posted names = %v", postedNames)
	}
}

func TestADOProviderRemovePullRequestLabel(t *testing.T) {
	// ADO 400s on delete-by-name when the name contains a colon (verified
	// live), so the provider resolves the label id via the /labels sub-endpoint
	// and deletes by id.
	const labelID = "ac1c1f66-a685-4e62-95da-b7b7ce927cb6"
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/labels", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"value": []map[string]interface{}{{"id": labelID, "name": "goobers:needs-remediation"}},
		})
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/labels/"+labelID, func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodDelete)
		if got := r.URL.Query().Get("api-version"); got != "7.1-preview.1" {
			t.Fatalf("api-version = %q, want 7.1-preview.1", got)
		}
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	if err := provider.RemovePullRequestLabel(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		"42",
		"goobers:needs-remediation",
	); err != nil {
		t.Fatalf("RemovePullRequestLabel returned error: %v", err)
	}
	if !deleted {
		t.Fatal("DELETE-by-id endpoint was not called")
	}
}

func TestADOProviderRemovePullRequestLabelAbsentIsBenign(t *testing.T) {
	// The label the caller asks to clear is not on the PR: the /labels lookup
	// returns an empty set and no DELETE is issued. Removal is a no-op, not an
	// error (mirrors GitHub's 404-is-benign label removal).
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/labels", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{}})
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/labels/", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("DELETE must not be issued for an absent label")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	if err := provider.RemovePullRequestLabel(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		"42",
		"goobers:needs-remediation",
	); err != nil {
		t.Fatalf("RemovePullRequestLabel on absent label returned error: %v, want nil (benign)", err)
	}
}

func TestADOProviderAuthenticatedLogin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		if got := r.URL.Query().Get("api-version"); got != "7.1-preview" {
			t.Fatalf("api-version = %q, want 7.1-preview", got)
		}
		writeJSON(t, w, map[string]interface{}{
			"authenticatedUser": map[string]interface{}{
				"id":                  "identity-guid",
				"providerDisplayName": "Goobers Bot",
				"properties":          map[string]interface{}{"Account": map[string]string{"$value": "bot@example.com"}},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	login, err := provider.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin returned error: %v", err)
	}
	if login != "Goobers Bot" {
		t.Fatalf("login = %q, want Goobers Bot (displayName, not UPN)", login)
	}
}

func TestADOProviderAuthenticatedLoginCustomDisplayNameFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"authenticatedUser": map[string]interface{}{
				"id":                "identity-guid",
				"customDisplayName": "Fallback Name",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	login, err := provider.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin returned error: %v", err)
	}
	if login != "Fallback Name" {
		t.Fatalf("login = %q, want Fallback Name", login)
	}
}

func TestADOProviderAuthenticatedLoginMissingIdentity(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"authenticatedUser": map[string]interface{}{"id": "identity-guid"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	if _, err := provider.AuthenticatedLogin(context.Background()); err == nil {
		t.Fatal("AuthenticatedLogin returned nil error for an identity with no display name")
	}
}

func TestADOProviderGetPullRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", prDetailHandler(t, []map[string]interface{}{{"vote": 10, "uniqueName": "reviewer@example.com"}}))
	mux.HandleFunc("/org/project/_apis/policy/evaluations", policyEvaluationsHandler(t, []map[string]interface{}{
		blockingPolicy("Build", "approved"),
		blockingPolicy("Status", "approved"),
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	summary, err := provider.GetPullRequest(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		"42",
	)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if summary.ID != "42" || summary.Number != 42 {
		t.Fatalf("unexpected identity: %#v", summary)
	}
	if summary.State != "open" || summary.Merged {
		t.Fatalf("unexpected state: %#v", summary)
	}
	if summary.Head != "goobers/implement/run-9" || summary.Base != "master" {
		t.Fatalf("unexpected branches: %#v", summary)
	}
	if summary.HeadSHA != "head-sha" || summary.BaseSHA != "base-sha" {
		t.Fatalf("unexpected refs: %#v", summary)
	}
	if summary.CheckState != CheckStatePassing {
		t.Fatalf("CheckState = %q, want passing", summary.CheckState)
	}
	if summary.Author != "mona@example.com" {
		t.Fatalf("Author = %q", summary.Author)
	}
	if summary.Body != "Implements PBI 100" {
		t.Fatalf("Body = %q", summary.Body)
	}
}
