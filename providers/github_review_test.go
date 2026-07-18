package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type reviewMutationRecorder struct {
	refs []ExternalRef
}

func (r *reviewMutationRecorder) RecordExternalRef(_ context.Context, ref ExternalRef) {
	r.refs = append(r.refs, ref)
}

func TestGitHubSubmitPullRequestReview(t *testing.T) {
	tests := []struct {
		name      string
		decision  ReviewDecision
		wantEvent string
		wantState string
	}{
		{name: "approve", decision: ReviewDecisionApproved, wantEvent: "APPROVE", wantState: "APPROVED"},
		{name: "request changes", decision: ReviewDecisionChangesRequested, wantEvent: "REQUEST_CHANGES", wantState: "CHANGES_REQUESTED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody map[string]string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/web/pulls/42/reviews" {
					t.Fatalf("request = %s %s, want POST /repos/acme/web/pulls/42/reviews", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id": 7, "html_url": "https://example/review/7",
					"commit_id": gotBody["commit_id"], "state": tt.wantState,
				})
			}))
			defer server.Close()

			recorder := &reviewMutationRecorder{}
			provider := NewGitHubProvider("token",
				func(p *GitHubProvider) { p.BaseURL = server.URL },
				WithMutationRecorder(recorder),
			)
			result, err := provider.SubmitPullRequestReview(context.Background(), PullRequestReviewRequest{
				Repository: RepositoryRef{Owner: "acme", Name: "web"},
				PullID:     "42",
				CommitSHA:  "head-sha",
				Decision:   tt.decision,
				Body:       "review body",
			})
			if err != nil {
				t.Fatalf("SubmitPullRequestReview: %v", err)
			}
			wantBody := map[string]string{
				"body": "review body", "commit_id": "head-sha", "event": tt.wantEvent,
			}
			if !reflect.DeepEqual(gotBody, wantBody) {
				t.Fatalf("body = %#v, want %#v", gotBody, wantBody)
			}
			if result.ID != 7 || result.URL != "https://example/review/7" ||
				result.CommitSHA != "head-sha" || result.Decision != tt.decision {
				t.Fatalf("result = %+v, want submitted review metadata", result)
			}
			if len(recorder.refs) != 1 || recorder.refs[0].Operation != "review" {
				t.Fatalf("recorded refs = %+v, want one review mutation", recorder.refs)
			}
		})
	}
}

func TestGitHubSubmitPullRequestReviewValidatesPinnedVerdict(t *testing.T) {
	provider := NewGitHubProvider("token")
	base := PullRequestReviewRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "web"},
		PullID:     "42",
		CommitSHA:  "head-sha",
		Decision:   ReviewDecisionApproved,
		Body:       "review body",
	}
	tests := []struct {
		name   string
		mutate func(*PullRequestReviewRequest)
	}{
		{name: "pull id", mutate: func(req *PullRequestReviewRequest) { req.PullID = "" }},
		{name: "commit sha", mutate: func(req *PullRequestReviewRequest) { req.CommitSHA = "" }},
		{name: "body", mutate: func(req *PullRequestReviewRequest) { req.Body = "" }},
		{name: "decision", mutate: func(req *PullRequestReviewRequest) { req.Decision = ReviewDecisionPending }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			if _, err := provider.SubmitPullRequestReview(context.Background(), req); err == nil {
				t.Fatal("SubmitPullRequestReview error = nil, want validation failure")
			}
		})
	}
}

var _ PullRequestReviewSubmitter = (*GitHubProvider)(nil)
