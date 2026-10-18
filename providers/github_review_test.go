package providers

import (
	"context"
	"encoding/json"
	"errors"
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
		{name: "comment without vote", decision: ReviewDecisionComment, wantEvent: "COMMENT", wantState: "COMMENTED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody map[string]string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/web/pulls/42/reviews" {
					t.Fatalf("request = %s %s, want /repos/acme/web/pulls/42/reviews", r.Method, r.URL.Path)
				}
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				if r.Method != http.MethodPost {
					t.Fatalf("request = %s %s, want POST or GET reviews", r.Method, r.URL.Path)
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

func TestGitHubApprovalDismissesOnlyOwnChangeRequests(t *testing.T) {
	var dismissed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/web/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 9, "html_url": "https://example/review/9",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/pulls/42/reviews":
			_, _ = w.Write([]byte(`[
				{"id":7,"state":"CHANGES_REQUESTED","user":{"login":"goobers-reviewer"},"html_url":"https://example/review/7"},
				{"id":8,"state":"CHANGES_REQUESTED","user":{"login":"human-reviewer"},"html_url":"https://example/review/8"},
				{"id":9,"state":"APPROVED","user":{"login":"goobers-reviewer"},"html_url":"https://example/review/9"}
			]`))
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"goobers-reviewer"}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/web/pulls/42/reviews/7/dismissals":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode dismissal: %v", err)
			}
			dismissed = append(dismissed, body["message"])
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	recorder := &reviewMutationRecorder{}
	provider := NewGitHubProvider("token",
		func(p *GitHubProvider) { p.BaseURL = server.URL },
		WithMutationRecorder(recorder),
	)
	if _, err := provider.SubmitPullRequestReview(context.Background(), PullRequestReviewRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "web"},
		PullID:     "42",
		CommitSHA:  "head-sha",
		Decision:   ReviewDecisionApproved,
		Body:       "resolved",
	}); err != nil {
		t.Fatalf("SubmitPullRequestReview: %v", err)
	}
	if len(dismissed) != 1 || dismissed[0] == "" {
		t.Fatalf("dismissals = %v, want one own-review dismissal with a reason", dismissed)
	}
	if len(recorder.refs) != 2 || recorder.refs[1].Operation != "review-dismiss" {
		t.Fatalf("recorded refs = %+v, want review followed by review-dismiss", recorder.refs)
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

func TestGitHubRequestReviewRecordsOrderedDigestAndRequiresPullID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/web/pulls/42/requested_reviewers" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	recorder := &reviewMutationRecorder{}
	provider := NewGitHubProvider("token",
		func(p *GitHubProvider) { p.BaseURL = server.URL },
		WithMutationRecorder(recorder),
	)
	repo := RepositoryRef{Owner: "acme", Name: "web"}
	if err := provider.RequestReview(context.Background(), ReviewRequest{
		Repository: repo, PullID: "42", Reviewers: []string{"zeta", "alpha"},
	}); err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	if len(recorder.refs) != 1 {
		t.Fatalf("recorded refs = %+v, want one request-review mutation", recorder.refs)
	}
	ref := recorder.refs[0]
	if ref.Operation != "request-review" || ref.Fields["reviewers"].After != digestString("zeta,alpha") {
		t.Fatalf("recorded ref = %+v, want caller-ordered reviewer digest", ref)
	}
	if err := provider.RequestReview(context.Background(), ReviewRequest{Repository: repo}); !errors.Is(err, errPullIDRequired) {
		t.Fatalf("missing PullID error = %v, want %v", err, errPullIDRequired)
	}
}

var _ PullRequestReviewSubmitter = (*GitHubProvider)(nil)
