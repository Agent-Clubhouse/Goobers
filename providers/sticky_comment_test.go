package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const stickyContractMarker = "<!-- goobers:cost:v1 -->"

func TestGitHubStickyCommentContract(t *testing.T) {
	type storedComment struct {
		ID      int64
		Body    string
		Created time.Time
	}
	var mu sync.Mutex
	var comments []storedComment
	var nextID int64 = 1
	ambiguous := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/issues/42/comments":
			out := make([]map[string]interface{}, 0, len(comments))
			for _, c := range comments {
				out = append(out, map[string]interface{}{
					"id": c.ID, "body": c.Body, "created_at": c.Created.Format(time.RFC3339),
					"user": map[string]string{"login": "goobers"},
				})
			}
			writeJSON(t, w, out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/web/issues/42/comments":
			var payload map[string]string
			decodeJSON(t, r, &payload)
			comment := storedComment{ID: nextID, Body: payload["body"], Created: time.Date(2026, time.September, 7, 1, int(nextID), 0, 0, time.UTC)}
			nextID++
			comments = append(comments, comment)
			if ambiguous {
				ambiguous = false
				http.Error(w, "response lost", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]interface{}{
				"id": comment.ID, "body": comment.Body, "created_at": comment.Created.Format(time.RFC3339),
				"user": map[string]string{"login": "goobers"},
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/web/issues/comments/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/acme/web/issues/comments/"), 10, 64)
			var payload map[string]string
			decodeJSON(t, r, &payload)
			for i := range comments {
				if comments[i].ID == id {
					comments[i].Body = payload["body"]
					writeJSON(t, w, map[string]interface{}{"id": id, "body": payload["body"]})
					return
				}
			}
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "web"}
	target := StickyCommentTarget{Kind: StickyCommentPullRequest, ID: "42"}
	assertStickyContract(t, provider, repo, target, &comments, &mu, func() { ambiguous = true })
}

func TestADOStickyPullRequestCommentContract(t *testing.T) {
	type storedComment struct {
		ThreadID int
		ID       int
		Body     string
		Created  time.Time
	}
	var mu sync.Mutex
	var comments []storedComment
	nextThread := 7
	ambiguous := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const base = "/org/project/_apis/git/repositories/repo/pullrequests/42/threads"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base:
			threads := make([]map[string]interface{}, 0, len(comments))
			for _, c := range comments {
				threads = append(threads, map[string]interface{}{
					"id": c.ThreadID,
					"comments": []map[string]interface{}{{
						"id": c.ID, "content": c.Body, "commentType": "text",
						"author":        map[string]string{"displayName": "Goobers"},
						"publishedDate": c.Created.Format(time.RFC3339),
					}},
				})
			}
			writeJSON(t, w, map[string]interface{}{"value": threads})
		case r.Method == http.MethodPost && r.URL.Path == base:
			var payload struct {
				Comments []struct {
					Content string `json:"content"`
				} `json:"comments"`
			}
			decodeJSON(t, r, &payload)
			comment := storedComment{
				ThreadID: nextThread, ID: 1, Body: payload.Comments[0].Content,
				Created: time.Date(2026, time.September, 7, 2, nextThread, 0, 0, time.UTC),
			}
			nextThread++
			comments = append(comments, comment)
			if ambiguous {
				ambiguous = false
				http.Error(w, "response lost", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]interface{}{
				"id": comment.ThreadID,
				"comments": []map[string]interface{}{{
					"id": comment.ID, "content": comment.Body, "commentType": "text",
					"author":        map[string]string{"displayName": "Goobers"},
					"publishedDate": comment.Created.Format(time.RFC3339),
				}},
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, base+"/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, base+"/"), "/")
			if len(parts) != 3 || parts[1] != "comments" {
				t.Fatalf("unexpected ADO patch path %s", r.URL.Path)
			}
			threadID, _ := strconv.Atoi(parts[0])
			commentID, _ := strconv.Atoi(parts[2])
			var payload map[string]string
			decodeJSON(t, r, &payload)
			for i := range comments {
				if comments[i].ThreadID == threadID && comments[i].ID == commentID {
					comments[i].Body = payload["content"]
					writeJSON(t, w, map[string]interface{}{"id": commentID, "content": payload["content"]})
					return
				}
			}
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected ADO request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Provider: ProviderADO, Owner: "org", Project: "project", Name: "repo"}
	target := StickyCommentTarget{Kind: StickyCommentPullRequest, ID: "42"}

	result, err := UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "first")
	if err != nil || !result.Created {
		t.Fatalf("create = %+v, %v", result, err)
	}
	result, err = UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "second")
	if err != nil || result.Created || result.Comment.ID != "42/7/1" {
		t.Fatalf("update = %+v, %v", result, err)
	}

	mu.Lock()
	comments = append(comments, storedComment{
		ThreadID: 99, ID: 1, Body: stickyContractMarker + "\nduplicate",
		Created: time.Date(2026, time.September, 7, 3, 0, 0, 0, time.UTC),
	})
	mu.Unlock()
	result, err = UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "third")
	if err != nil || len(result.DuplicateIDs) != 1 || result.DuplicateIDs[0] != "42/99/1" || result.Comment.ID != "42/7/1" {
		t.Fatalf("duplicate resolution = %+v, %v", result, err)
	}

	mu.Lock()
	comments = nil
	ambiguous = true
	mu.Unlock()
	result, err = UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "adopted")
	if err != nil || !result.Adopted || result.Created {
		t.Fatalf("ambiguous adoption = %+v, %v", result, err)
	}
}

func TestADOStickyIssueCommentUsesWorkItemTransport(t *testing.T) {
	body := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			comments := []map[string]interface{}{}
			if body != "" {
				comments = append(comments, map[string]interface{}{
					"id": 5, "text": body, "createdBy": map[string]string{"displayName": "Goobers"},
					"createdDate": "2026-09-07T02:00:00Z",
				})
			}
			writeJSON(t, w, map[string]interface{}{"comments": comments})
		case http.MethodPost:
			var payload map[string]string
			decodeJSON(t, r, &payload)
			body = payload["text"]
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]interface{}{"id": 5, "text": body})
		default:
			t.Fatalf("unexpected ADO work-item request %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/comments/5", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		var payload map[string]string
		decodeJSON(t, r, &payload)
		body = payload["text"]
		writeJSON(t, w, map[string]interface{}{"id": 5, "text": body})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Provider: ProviderADO, Owner: "org", Project: "project", Name: "repo"}
	target := StickyCommentTarget{Kind: StickyCommentIssue, ID: "42"}
	first, err := UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "first")
	if err != nil || !first.Created {
		t.Fatalf("create issue sticky = %+v, %v", first, err)
	}
	second, err := UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "second")
	if err != nil || second.Created || !strings.Contains(body, "second") {
		t.Fatalf("update issue sticky = %+v, %v, body %q", second, err, body)
	}
}

func assertStickyContract(
	t *testing.T,
	provider Provider,
	repo RepositoryRef,
	target StickyCommentTarget,
	comments interface{},
	mu *sync.Mutex,
	setAmbiguous func(),
) {
	t.Helper()
	result, err := UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "first")
	if err != nil || !result.Created {
		t.Fatalf("create = %+v, %v", result, err)
	}
	result, err = UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "second")
	if err != nil || result.Created || result.Comment.ID != "1" {
		t.Fatalf("update = %+v, %v", result, err)
	}

	mu.Lock()
	raw, _ := json.Marshal(comments)
	var current []struct {
		ID      int64
		Body    string
		Created time.Time
	}
	_ = json.Unmarshal(raw, &current)
	current = append(current, struct {
		ID      int64
		Body    string
		Created time.Time
	}{ID: 99, Body: stickyContractMarker + "\nduplicate", Created: time.Date(2026, time.September, 7, 3, 0, 0, 0, time.UTC)})
	encoded, _ := json.Marshal(current)
	_ = json.Unmarshal(encoded, comments)
	mu.Unlock()

	result, err = UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "third")
	if err != nil || len(result.DuplicateIDs) != 1 || result.DuplicateIDs[0] != "99" || result.Comment.ID != "1" {
		t.Fatalf("duplicate resolution = %+v, %v", result, err)
	}

	mu.Lock()
	empty := []struct {
		ID      int64
		Body    string
		Created time.Time
	}{}
	encoded, _ = json.Marshal(empty)
	_ = json.Unmarshal(encoded, comments)
	setAmbiguous()
	mu.Unlock()
	result, err = UpsertStickyComment(context.Background(), provider, repo, target, stickyContractMarker, "adopted")
	if err != nil || !result.Adopted || result.Created {
		t.Fatalf("ambiguous adoption = %+v, %v", result, err)
	}
}
