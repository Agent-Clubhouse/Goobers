package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubListPullRequestReviewThreadsPreservesBodiesAnchorsAndState(t *testing.T) {
	var server *httptest.Server
	graphqlCalls := 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/pulls/42/reviews":
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`[{"id":2,"user":{"login":"human"},"state":"APPROVED","body":"Looks good.","commit_id":"head-2","submitted_at":"2026-07-23T11:00:00Z","html_url":"https://example/reviews/2"}]`))
				return
			}
			if r.URL.Query().Get("per_page") != "100" {
				t.Fatalf("reviews per_page = %q, want 100", r.URL.Query().Get("per_page"))
			}
			w.Header().Set("Link", "<"+server.URL+r.URL.Path+"?page=2&per_page=100>; rel=\"next\"")
			_, _ = w.Write([]byte(`[{"id":1,"user":{"login":"goobers-bot"},"state":"CHANGES_REQUESTED","body":"Fix both findings.","commit_id":"head-1","submitted_at":"2026-07-23T10:00:00Z","html_url":"https://example/reviews/1"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/pulls/42/comments":
			_, _ = w.Write([]byte(`[
				{"id":101,"user":{"login":"alice"},"body":"Guard this write.","path":"worker.go","line":42,"original_line":40,"side":"RIGHT","start_line":40,"original_start_line":38,"start_side":"RIGHT","diff_hunk":"@@ -38,3 +40,5 @@","created_at":"2026-07-23T10:05:00Z","html_url":"https://example/comments/101"},
				{"id":102,"user":{"login":"bob"},"body":"Still reproducible.","path":"worker.go","line":42,"original_line":40,"side":"RIGHT","diff_hunk":"@@ -38,3 +40,5 @@","in_reply_to_id":101,"created_at":"2026-07-23T10:06:00Z","html_url":"https://example/comments/102"},
				{"id":201,"user":{"login":"carol"},"body":"Old location.","path":"legacy.go","line":null,"original_line":9,"side":"RIGHT","diff_hunk":"@@ -8,2 +8,2 @@","created_at":"2026-07-23T09:00:00Z","html_url":"https://example/comments/201"}
			]`))
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var request struct {
				Query     string                 `json:"query"`
				Variables map[string]interface{} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode GraphQL request: %v", err)
			}
			if !strings.Contains(request.Query, "reviewThreads") {
				t.Fatalf("GraphQL query does not request reviewThreads: %q", request.Query)
			}
			graphqlCalls++
			if graphqlCalls == 1 {
				if request.Variables["after"] != nil {
					t.Fatalf("first reviewThreads cursor = %v, want nil", request.Variables["after"])
				}
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
					{"isResolved":false,"isOutdated":false,"path":"worker.go","line":42,"originalLine":40,"diffSide":"RIGHT","startLine":40,"originalStartLine":38,"startDiffSide":"RIGHT","comments":{"nodes":[{"databaseId":101},{"databaseId":102}]}}
				],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`))
				return
			}
			if request.Variables["after"] != "cursor-1" {
				t.Fatalf("second reviewThreads cursor = %v, want cursor-1", request.Variables["after"])
			}
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
				{"isResolved":true,"isOutdated":true,"path":"legacy.go","line":null,"originalLine":9,"diffSide":"RIGHT","comments":{"nodes":[{"databaseId":201}]}}
			],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	got, err := provider.ListPullRequestReviewThreads(
		context.Background(),
		RepositoryRef{Owner: "acme", Name: "web"},
		"42",
	)
	if err != nil {
		t.Fatalf("ListPullRequestReviewThreads: %v", err)
	}
	if graphqlCalls != 2 {
		t.Fatalf("reviewThreads GraphQL calls = %d, want 2 paginated requests", graphqlCalls)
	}
	if len(got.Reviews) != 2 {
		t.Fatalf("reviews = %d, want 2", len(got.Reviews))
	}
	if review := got.Reviews[0]; review.Author != "goobers-bot" ||
		review.State != "CHANGES_REQUESTED" || review.Body != "Fix both findings." ||
		review.CommitSHA != "head-1" || review.SubmittedAt == nil {
		t.Fatalf("apply-verdict review = %+v, want complete native review body", review)
	}
	if len(got.InlineComments) != 3 {
		t.Fatalf("inline comments = %d, want 3", len(got.InlineComments))
	}
	live := got.InlineComments[0]
	if live.Path != "worker.go" || live.Line != 42 || live.OriginalLine != 40 ||
		live.Side != "RIGHT" || live.StartLine != 40 || live.OriginalStartLine != 38 ||
		live.StartSide != "RIGHT" || live.DiffHunk != "@@ -38,3 +40,5 @@" ||
		live.IsResolved || live.IsOutdated {
		t.Fatalf("live inline comment = %+v, want intact live anchor", live)
	}
	reply := got.InlineComments[1]
	if reply.InReplyTo != 101 || reply.StartLine != 40 || reply.OriginalStartLine != 38 ||
		reply.StartSide != "RIGHT" || reply.IsResolved || reply.IsOutdated {
		t.Fatalf("reply = %+v, want live parent thread state", reply)
	}
	outdated := got.InlineComments[2]
	if outdated.Line != 0 || outdated.OriginalLine != 9 ||
		!outdated.IsResolved || !outdated.IsOutdated {
		t.Fatalf("outdated inline comment = %+v, want distinguishable resolved/outdated state", outdated)
	}
}

func TestGitHubListPullRequestReviewThreadsFailsWhenThreadStateIsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/web/pulls/42/reviews":
			_, _ = w.Write([]byte(`[]`))
		case "/graphql":
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
		case "/repos/acme/web/pulls/42/comments":
			_, _ = w.Write([]byte(`[{"id":101,"body":"unknown state","path":"worker.go"}]`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	_, err := provider.ListPullRequestReviewThreads(
		context.Background(),
		RepositoryRef{Owner: "acme", Name: "web"},
		"42",
	)
	if err == nil || !strings.Contains(err.Error(), "no review-thread state") {
		t.Fatalf("error = %v, want missing thread state failure", err)
	}
}
