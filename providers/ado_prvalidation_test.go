package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// prDetailHandler serves a single-PR GET with the reviewers and project identity
// PollPullRequest needs to key policy evaluations.
func prDetailHandler(t *testing.T, reviewers []map[string]interface{}) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId":         42,
			"status":                "active",
			"title":                 "Implement PBI 100",
			"description":           "Implements PBI 100",
			"createdBy":             map[string]string{"displayName": "Mona", "uniqueName": "mona@example.com"},
			"isDraft":               false,
			"sourceRefName":         "refs/heads/goobers/implement/run-9",
			"targetRefName":         "refs/heads/master",
			"lastMergeSourceCommit": map[string]string{"commitId": "head-sha"},
			"lastMergeTargetCommit": map[string]string{"commitId": "base-sha"},
			"reviewers":             reviewers,
			"repository": map[string]interface{}{
				"id":      "repo-guid",
				"name":    "repo",
				"project": map[string]string{"id": "proj-guid", "name": "project"},
			},
		})
	}
}

func policyEvaluationsHandler(t *testing.T, evaluations []map[string]interface{}) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		if got := r.URL.Query().Get("scopeId"); got != "proj-guid" {
			t.Fatalf("scopeId = %q, want proj-guid", got)
		}
		if got := r.URL.Query().Get("artifactId"); got != "vstfs:///CodeReview/CodeReviewId/proj-guid/42" {
			t.Fatalf("artifactId = %q", got)
		}
		writeJSON(t, w, map[string]interface{}{"value": evaluations})
	}
}

func blockingPolicy(displayName, status string) map[string]interface{} {
	return blockingPolicyID(0, displayName, status)
}

// blockingPolicyID builds a blocking policy evaluation carrying a configuration
// id (ADO returns it as a JSON number). id 0 omits it.
func blockingPolicyID(id int, displayName, status string) map[string]interface{} {
	cfg := map[string]interface{}{
		"isEnabled":  true,
		"isBlocking": true,
		"type":       map[string]string{"displayName": displayName},
	}
	if id != 0 {
		cfg["id"] = id
	}
	return map[string]interface{}{
		"status":        status,
		"configuration": cfg,
	}
}

func TestADOProviderPollPullRequestPolicyEvaluations(t *testing.T) {
	cases := []struct {
		name           string
		reviewers      []map[string]interface{}
		evaluations    []map[string]interface{}
		humanOnly      []string
		wantState      CheckState
		wantReview     ReviewDecision
		wantCheckNames []string
	}{
		{
			name: "all gating policies approved is passing",
			reviewers: []map[string]interface{}{
				{"vote": 10, "uniqueName": "done@example.com"},
				{"vote": 0, "uniqueName": "pending@example.com"},
			},
			evaluations: []map[string]interface{}{
				blockingPolicy("Build", "approved"),
				blockingPolicy("Status", "approved"),
				// A non-blocking rejected policy must be ignored (advisory).
				{"status": "rejected", "configuration": map[string]interface{}{"isEnabled": true, "isBlocking": false, "type": map[string]string{"displayName": "Optional"}}},
			},
			wantState:      CheckStatePassing,
			wantReview:     ReviewDecisionApproved,
			wantCheckNames: []string{"Build", "Status"},
		},
		{
			name:      "a rejected gating policy is failing",
			reviewers: []map[string]interface{}{{"vote": 10}},
			evaluations: []map[string]interface{}{
				blockingPolicy("Build", "rejected"),
				blockingPolicy("Status", "approved"),
			},
			wantState:      CheckStateFailing,
			wantReview:     ReviewDecisionApproved,
			wantCheckNames: []string{"Build", "Status"},
		},
		{
			// The core config-driven fix: a rejected human/merge-time policy
			// declared human-only by its configuration id must NOT fail the gate
			// when the gating (agent-fixable) policies are green — otherwise
			// ci-poll pegs to failing forever and the fix loop can never reach
			// the hand-off-to-human terminal.
			name:      "rejected human-only policy does not fail the gate when gating is green",
			reviewers: []map[string]interface{}{{"vote": -10}},
			evaluations: []map[string]interface{}{
				blockingPolicyID(10, "Build", "approved"),
				blockingPolicyID(77, "Require a merge strategy", "rejected"),
			},
			humanOnly:      []string{"77"},
			wantState:      CheckStatePassing,
			wantReview:     ReviewDecisionChangesRequested,
			wantCheckNames: []string{"Build", "Require a merge strategy"},
		},
		{
			name:      "a queued gating policy is pending",
			reviewers: nil,
			evaluations: []map[string]interface{}{
				blockingPolicy("Build", "queued"),
				blockingPolicy("Status", "approved"),
			},
			wantState:      CheckStatePending,
			wantReview:     ReviewDecisionPending,
			wantCheckNames: []string{"Build", "Status"},
		},
		{
			// Every required policy is human-only: no gating policy remains, so
			// fail-closed to pending (keep polling / eventually escalate) rather
			// than a false pass on a build correctness that was never proven.
			name:      "all policies human-only is pending (fail-closed)",
			reviewers: []map[string]interface{}{{"vote": 10}},
			evaluations: []map[string]interface{}{
				blockingPolicyID(55, "Require a merge strategy", "approved"),
			},
			humanOnly:      []string{"55"},
			wantState:      CheckStatePending,
			wantReview:     ReviewDecisionApproved,
			wantCheckNames: []string{"Require a merge strategy"},
		},
		{
			name:           "no blocking policies is pending (fail-closed)",
			reviewers:      []map[string]interface{}{{"vote": 10}},
			evaluations:    []map[string]interface{}{},
			wantState:      CheckStatePending,
			wantReview:     ReviewDecisionApproved,
			wantCheckNames: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", prDetailHandler(t, tc.reviewers))
			mux.HandleFunc("/org/project/_apis/policy/evaluations", policyEvaluationsHandler(t, tc.evaluations))
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			result, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
				Repository:                  RepositoryRef{Name: "repo", Project: "project"},
				PullID:                      "42",
				HumanPolicyConfigurationIDs: tc.humanOnly,
			})
			if err != nil {
				t.Fatalf("PollPullRequest returned error: %v", err)
			}
			if result.Number != 42 || result.State != "open" || result.Merged {
				t.Fatalf("unexpected identity/state: %#v", result)
			}
			if result.Author != "mona@example.com" {
				t.Fatalf("Author = %q, want mona@example.com", result.Author)
			}
			if tc.name == "all gating policies approved is passing" &&
				(len(result.RequestedReviewers) != 1 || result.RequestedReviewers[0] != "pending@example.com") {
				t.Fatalf("RequestedReviewers = %v, want only the unvoted reviewer", result.RequestedReviewers)
			}
			if result.HeadSHA != "head-sha" || result.BaseSHA != "base-sha" || result.BaseBranch != "master" {
				t.Fatalf("unexpected refs: %#v", result)
			}
			if result.CheckState != tc.wantState {
				t.Fatalf("CheckState = %q, want %q", result.CheckState, tc.wantState)
			}
			if result.ReviewDecision != tc.wantReview {
				t.Fatalf("ReviewDecision = %q, want %q", result.ReviewDecision, tc.wantReview)
			}
			if result.Integrity != apiintegrity.Unapproved {
				t.Fatalf("Integrity = %q, want unapproved", result.Integrity)
			}
			gotNames := make([]string, 0, len(result.Checks))
			for _, c := range result.Checks {
				gotNames = append(gotNames, c.Name)
			}
			if len(gotNames) != len(tc.wantCheckNames) {
				t.Fatalf("check names = %v, want %v", gotNames, tc.wantCheckNames)
			}
			for i, want := range tc.wantCheckNames {
				if gotNames[i] != want {
					t.Fatalf("check names = %v, want %v", gotNames, tc.wantCheckNames)
				}
			}
		})
	}
}

func TestADOProviderPollPullRequestProviderError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", prDetailHandler(t, nil))
	mux.HandleFunc("/org/project/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "policy service unavailable", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	// The assertion is "the 500 surfaces as an error", not "the retries were
	// timed": stub the backoff sleep (the in-package idiom, cf.
	// ado_landing_test.go) so the retry ladder still runs but costs no real
	// wall-clock time instead of 1+2+4+8 = 15s.
	provider.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "42",
	})
	if err == nil {
		t.Fatal("PollPullRequest returned nil error")
	}
}

func TestADOProviderPublishPullRequestStatus(t *testing.T) {
	var captured map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/statuses", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		writeJSON(t, w, map[string]interface{}{"id": 7})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.PublishPullRequestStatus(context.Background(), PullRequestStatusRequest{
		Repository:  RepositoryRef{Name: "repo", Project: "project"},
		PullID:      "42",
		Name:        "review",
		State:       CheckStatePassing,
		Description: "reviewer approved",
		TargetURL:   "http://example/run",
	})
	if err != nil {
		t.Fatalf("PublishPullRequestStatus returned error: %v", err)
	}
	if result.ID != 7 {
		t.Fatalf("status id = %d, want 7", result.ID)
	}
	if captured["state"] != "succeeded" {
		t.Fatalf("state = %v, want succeeded", captured["state"])
	}
	if captured["targetUrl"] != "http://example/run" {
		t.Fatalf("targetUrl = %v", captured["targetUrl"])
	}
	ctx, ok := captured["context"].(map[string]interface{})
	if !ok || ctx["genre"] != "goobers" || ctx["name"] != "review" {
		t.Fatalf("context = %#v, want genre=goobers name=review", captured["context"])
	}
}

func TestADOProviderClosePullRequestAbandons(t *testing.T) {
	var captured map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		writeJSON(t, w, map[string]interface{}{"pullRequestId": 42, "status": "abandoned"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.ClosePullRequest(context.Background(), ClosePullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "42",
	})
	if err != nil {
		t.Fatalf("ClosePullRequest returned error: %v", err)
	}
	if captured["status"] != "abandoned" {
		t.Fatalf("status = %v, want abandoned", captured["status"])
	}
	if result.Number != 42 || result.Merged || result.State != "closed" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestADOProviderClosePullRequestFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		http.Error(w, "close failed", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.ClosePullRequest(context.Background(), ClosePullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "42",
	})
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("ClosePullRequest error = %v, want status 500", err)
	}
}
