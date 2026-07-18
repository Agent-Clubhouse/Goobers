package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsSelfReviewError(t *testing.T) {
	approveBody := `{"message":"Unprocessable Entity","errors":["Review Can not approve your own pull request"],"documentation_url":"https://docs.github.com/rest/pulls/reviews#create-a-review-for-a-pull-request"}`
	requestChangesBody := `{"message":"Unprocessable Entity","errors":["Review Can not request changes on your own pull request"]}`

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "typed 422 approve self-review",
			err:  &providerResponseError{statusCode: http.StatusUnprocessableEntity, body: approveBody},
			want: true,
		},
		{
			name: "typed 422 request-changes self-review",
			err:  &providerResponseError{statusCode: http.StatusUnprocessableEntity, body: requestChangesBody},
			want: true,
		},
		{
			name: "typed 422 unrelated validation error",
			err:  &providerResponseError{statusCode: http.StatusUnprocessableEntity, body: `{"message":"Validation Failed","errors":["A pull request already exists"]}`},
			want: false,
		},
		{
			name: "typed 403 with self-review-ish body is not a self-review error",
			err:  &providerResponseError{statusCode: http.StatusForbidden, body: approveBody},
			want: false,
		},
		{
			name: "wrapped typed error still detected",
			err:  fmt.Errorf("submit native review for PR #828: %w", &providerResponseError{statusCode: http.StatusUnprocessableEntity, body: approveBody}),
			want: true,
		},
		{
			name: "subprocess-crossed stringified error",
			err:  errors.New(`submit native review for PR #828: POST https://api.github.com/repos/o/r/pulls/828/reviews failed: status 422: {"errors":["Review Can not approve your own pull request"]}`),
			want: true,
		},
		{
			name: "stringified 422 without the self-review marker",
			err:  errors.New("POST /pulls/1/reviews failed: status 422: validation failed"),
			want: false,
		},
		{
			name: "transient 500 is not a self-review error",
			err:  errors.New("status 500: server error your own pull request"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSelfReviewError(tt.err); got != tt.want {
				t.Fatalf("IsSelfReviewError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsSelfReviewErrorFromLiveProvider proves the predicate fires on the
// actual error SubmitPullRequestReview returns when the server replies with
// GitHub's real self-review 422 — the value apply-verdict inspects, not a
// hand-built one.
func TestIsSelfReviewErrorFromLiveProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprint(w, `{"message":"Unprocessable Entity","errors":["Review Can not approve your own pull request"]}`)
	}))
	defer server.Close()

	p := NewGitHubProvider("tok", func(p *GitHubProvider) { p.BaseURL = server.URL })
	_, err := p.SubmitPullRequestReview(context.Background(), PullRequestReviewRequest{
		Repository: RepositoryRef{Owner: "o", Name: "r"},
		PullID:     "828",
		CommitSHA:  "deadbeef",
		Decision:   ReviewDecisionApproved,
		Body:       "verdict",
	})
	if err == nil {
		t.Fatal("SubmitPullRequestReview error = nil, want the self-review 422")
	}
	if !IsSelfReviewError(err) {
		t.Fatalf("IsSelfReviewError(%v) = false, want true", err)
	}
}
