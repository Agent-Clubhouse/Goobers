package providerscontract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/providers"
)

// landingBackend wires a provider to a mock server reporting one pull
// request's live merge-queue state, for the pr.landing.poll cross-provider
// contract (CONF-4, #2077).
type landingBackend struct {
	provider providers.MergeQueuePoller
	repo     providers.RepositoryRef
}

// newGitHubLandingBackend serves pull request 42 over GitHub's GraphQL
// endpoint (the only surface PollMergeQueueEntry reads — see
// providers/github.go's pollMergeQueueEntryQuery) in the shape `state`
// selects.
func newGitHubLandingBackend(t *testing.T, state string) landingBackend {
	t.Helper()
	pr := map[string]interface{}{
		"id": "PR_kwID", "state": "OPEN", "merged": false,
		"mergeCommit":     nil,
		"mergeQueueEntry": nil,
		"labels":          map[string]interface{}{"nodes": []interface{}{}},
	}
	switch state {
	case "merged":
		pr["merged"] = true
		pr["mergeCommit"] = map[string]interface{}{"oid": "merged-sha"}
	case "pending":
		pr["mergeQueueEntry"] = map[string]interface{}{"state": "QUEUED", "position": 1}
	case "evicted":
		// GitHub leaves an evicted PR open but closes it once the queue
		// abandons it outright; PollMergeQueueEntry classifies "closed
		// without merging" as Evicted regardless of how it got there.
		pr["state"] = "CLOSED"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode graphql request: %v", err)
		}
		writeJSON(t, w, map[string]interface{}{
			"data": map[string]interface{}{
				"repository": map[string]interface{}{"pullRequest": pr},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) { p.BaseURL = srv.URL })
	return landingBackend{provider: p, repo: providers.RepositoryRef{Owner: "acme", Name: "app"}}
}

// newADOLandingBackend serves pull request 42 over ADO's single-PR GET (the
// surface PollMergeQueueEntry reads — see providers/ado_landing.go) in the
// shape `state` selects.
func newADOLandingBackend(t *testing.T, state string) landingBackend {
	t.Helper()
	resp := map[string]interface{}{"pullRequestId": 42, "status": "active"}
	switch state {
	case "merged":
		resp["status"] = "completed"
		resp["lastMergeCommit"] = map[string]string{"commitId": "merged-sha"}
	case "pending":
		resp["autoCompleteSetBy"] = map[string]string{"id": "someone"}
	case "evicted":
		// Active with no auto-complete armed: ADO cleared it (policy
		// rejection or manual clear) — no separate "abandoned" needed to
		// pin the same Evicted outcome GitHub's closed-without-merge case
		// reports.
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = srv.URL })
	return landingBackend{provider: p, repo: providers.RepositoryRef{Name: "repo", Project: "project"}}
}

// TestContract_PollMergeQueueEntryLandedOracle pins design doc §4's core
// landing invariant identically for both blessed providers: pr.landing.poll
// is the SOLE landed-oracle, and merged/pending/evicted must classify the
// same way regardless of provider — merge-review's repass loop reads this
// result without knowing which provider it came from. GitHub also reports
// a fourth state, Absent, for a real ambiguity (open, unmerged, no queue
// entry — indistinguishable from "not yet enqueued") that ADO's polling
// model doesn't share (see MergeQueueEntryAbsent's doc); that state is
// intentionally excluded from this cross-provider pin, the same way
// GitHub-only capabilities like pr.review.threads are.
func TestContract_PollMergeQueueEntryLandedOracle(t *testing.T) {
	tests := []struct {
		state     string
		wantState providers.MergeQueueEntryState
		wantSHA   string
	}{
		{"merged", providers.MergeQueueEntryMerged, "merged-sha"},
		{"pending", providers.MergeQueueEntryPending, ""},
		{"evicted", providers.MergeQueueEntryEvicted, ""},
	}
	backends := []struct {
		name string
		make func(*testing.T, string) landingBackend
	}{
		{"github", newGitHubLandingBackend},
		{"ado", newADOLandingBackend},
	}
	for _, tc := range tests {
		for _, bf := range backends {
			t.Run(bf.name+"/"+tc.state, func(t *testing.T) {
				b := bf.make(t, tc.state)
				result, err := b.provider.PollMergeQueueEntry(context.Background(), providers.PollMergeQueueEntryRequest{
					Repository: b.repo, PullID: "42",
				})
				if err != nil {
					t.Fatalf("PollMergeQueueEntry: %v", err)
				}
				if result.State != tc.wantState {
					t.Fatalf("State = %q, want %q", result.State, tc.wantState)
				}
				if result.MergeSHA != tc.wantSHA {
					t.Fatalf("MergeSHA = %q, want %q", result.MergeSHA, tc.wantSHA)
				}
			})
		}
	}
}
