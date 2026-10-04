package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestADOProviderPollMergeQueueEntryAwaitingHumanIsBestEffort proves the
// policy-evaluations read behind AwaitingHuman is diagnostic only: when it
// fails (a 403 on the preview endpoint, or a 500 that outlives its retries)
// an armed, active pull request is still reported plainly pending rather than
// failing the landing watch.
func TestADOProviderPollMergeQueueEntryAwaitingHumanIsBestEffort(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]interface{}{
					"pullRequestId": 42, "status": "active",
					"autoCompleteSetBy": map[string]string{"id": "someone"},
					"repository": map[string]interface{}{
						"name": "repo", "project": map[string]string{"id": "proj-guid", "name": "project"},
					},
				})
			})
			mux.HandleFunc("/org/project/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "evaluations unavailable", status)
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			provider.sleep = func(context.Context, time.Duration) error { return nil }
			result, err := provider.PollMergeQueueEntry(context.Background(), PollMergeQueueEntryRequest{Repository: adoLandingRepo(), PullID: "42"})
			if err != nil {
				t.Fatalf("PollMergeQueueEntry returned error %v, want a plain pending entry", err)
			}
			if result.State != MergeQueueEntryPending || result.AwaitingHuman {
				t.Fatalf("result = %+v, want pending and not awaiting a human", result)
			}
		})
	}
}

// TestADOProviderDetectMergePolicyDefaultBranchScope proves a policy scoped to
// "the default branch of each repository" (matchKind DefaultBranch, no
// refName) gates only that branch: a pull request into any other branch is
// not misdetected as auto-complete-gated. When the default branch cannot be
// read, the scope still gates, as it did before.
func TestADOProviderDetectMergePolicyDefaultBranchScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		branch     string
		repoStatus int
		want       MergePolicy
	}{
		{name: "default branch is gated", branch: "main", repoStatus: http.StatusOK, want: MergePolicyMergeQueue},
		{name: "other branch is not gated", branch: "release/1.0", repoStatus: http.StatusOK, want: MergePolicyDirect},
		{name: "unknown default branch stays conservative", branch: "release/1.0", repoStatus: http.StatusNotFound, want: MergePolicyMergeQueue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo", func(w http.ResponseWriter, _ *http.Request) {
				if tc.repoStatus != http.StatusOK {
					http.Error(w, "not found", tc.repoStatus)
					return
				}
				writeJSON(t, w, map[string]interface{}{
					"id": "repo-guid", "defaultBranch": "refs/heads/main",
					"project": map[string]string{"id": "proj-guid"},
				})
			})
			mux.HandleFunc("/org/project/_apis/policy/configurations", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
					blockingConfig(map[string]string{"matchKind": "DefaultBranch"}),
				}})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			result, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{Repository: adoLandingRepo(), Branch: tc.branch})
			if err != nil {
				t.Fatalf("DetectMergePolicy returned error: %v", err)
			}
			if result.Policy != tc.want {
				t.Fatalf("Policy = %q, want %q", result.Policy, tc.want)
			}
		})
	}
}

// TestADOProviderRequestReviewRefusesAmbiguousAndPartial proves RequestReview
// never guesses between several matching identities, and resolves every
// reviewer before adding any: one reviewer that fails leaves the pull request
// without a partial reviewer set.
func TestADOProviderRequestReviewRefusesAmbiguousAndPartial(t *testing.T) {
	const reviewerGUID = "22222222-2222-2222-2222-222222222222"
	puts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/org/_apis/identities", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("filterValue") {
		case "reviewer@example-org.com":
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{{"id": reviewerGUID}, {"id": reviewerGUID}}})
		case "Shared Name":
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{
				{"id": "44444444-4444-4444-4444-444444444444"},
				{"id": "55555555-5555-5555-5555-555555555555"},
			}})
		default:
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{}})
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12/reviewers/", func(w http.ResponseWriter, _ *http.Request) {
		puts++
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}

	err := provider.RequestReview(context.Background(), ReviewRequest{Repository: repo, PullID: "12", Reviewers: []string{"Shared Name"}})
	if err == nil || !strings.Contains(err.Error(), "matched 2 identities") {
		t.Fatalf("err = %v, want an ambiguous-reviewer refusal", err)
	}
	err = provider.RequestReview(context.Background(), ReviewRequest{
		Repository: repo, PullID: "12", Reviewers: []string{"reviewer@example-org.com", "nobody@example-org.com"},
	})
	if err == nil || !strings.Contains(err.Error(), "did not resolve") {
		t.Fatalf("err = %v, want the unresolvable reviewer's error", err)
	}
	if puts != 0 {
		t.Fatalf("reviewer PUTs = %d, want 0 when any reviewer fails to resolve", puts)
	}
	// The same identity returned twice is one match, not an ambiguity.
	if err := provider.RequestReview(context.Background(), ReviewRequest{Repository: repo, PullID: "12", Reviewers: []string{"reviewer@example-org.com"}}); err != nil {
		t.Fatalf("RequestReview returned error: %v", err)
	}
	if puts != 1 {
		t.Fatalf("reviewer PUTs = %d, want 1", puts)
	}
}
