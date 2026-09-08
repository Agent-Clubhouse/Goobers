package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGiteaSubmitReviewCommentDoesNotVote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		if r.URL.Path != "/api/v1/repos/acme/app/pulls/9/reviews" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]string
		decodeJSON(t, r, &body)
		if body["event"] != "COMMENT" || body["commit_id"] != "head123" || body["body"] != "Waiting for a sibling." {
			t.Errorf("comment changed vote, pin, or evidence: %+v", body)
		}
		writeJSON(t, w, map[string]any{"id": 42, "html_url": "review-url"})
	}))
	defer server.Close()
	result, err := NewGiteaProvider(server.URL, "token").SubmitPullRequestReview(context.Background(), PullRequestReviewRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", CommitSHA: "head123",
		Decision: ReviewDecisionComment, Body: "Waiting for a sibling.",
	})
	if err != nil || result.Decision != ReviewDecisionComment || result.CommitSHA != "head123" {
		t.Fatalf("comment publication: result=%+v err=%v", result, err)
	}
}
