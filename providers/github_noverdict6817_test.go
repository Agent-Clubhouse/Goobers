package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// outageCheckRuns is the shape a hosted-runner outage leaves on a head: every
// job waited for a runner, got none, and was cancelled with zero steps run.
func outageCheckRuns() []map[string]interface{} {
	return []map[string]interface{}{
		checkRunFixture(101, "build", actionsAppID, "completed", "cancelled"),
		checkRunFixture(102, "test", actionsAppID, "completed", "cancelled"),
		checkRunFixture(103, "lint", actionsAppID, "completed", "cancelled"),
	}
}

func outageServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"number": 7, "state": "open", "mergeable_state": "blocked",
			"head": map[string]interface{}{"ref": "feature", "sha": "deadbeef"},
			"base": map[string]interface{}{"ref": "main", "sha": "base"},
		})
	})
	mux.HandleFunc("/repos/acme/app/pulls/7/reviews", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []map[string]interface{}{})
	})
	mux.HandleFunc("/repos/acme/app/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []map[string]interface{}{})
	})
	mux.HandleFunc("/repos/acme/app/commits/deadbeef/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"statuses": []map[string]interface{}{}})
	})
	mux.HandleFunc("/repos/acme/app/commits/deadbeef/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"check_runs": outageCheckRuns()})
	})
	mux.HandleFunc("/repos/acme/app/check-runs/{id}/annotations", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fetched annotations for no-verdict check run %s", r.PathValue("id"))
		writeJSON(t, w, []map[string]interface{}{})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestGitHubProviderOutageIsNotFailingCI is #6817's provider acceptance: a
// head whose every check was cancelled before running reports no CI failure
// (so gather-ci-failures has nothing to hand an agent) and polls as pending,
// never passing (so no merge gate accepts it).
func TestGitHubProviderOutageIsNotFailingCI(t *testing.T) {
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = outageServer(t).URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}

	failures, err := provider.CIFailures(context.Background(), repo, "deadbeef")
	if err != nil {
		t.Fatalf("CIFailures: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("CIFailures = %+v, want none for an outage-cancelled head", failures)
	}

	poll, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: repo, PullID: "7"})
	if err != nil {
		t.Fatalf("PollPullRequest: %v", err)
	}
	if poll.CheckState != CheckStatePending {
		t.Fatalf("poll CheckState = %q, want pending: never failing (no remediation), never passing (no merge)", poll.CheckState)
	}
	for _, check := range poll.Checks {
		if check.State != CheckStatePending || check.Conclusion != "cancelled" || !check.NoVerdict {
			t.Fatalf("check %+v, want pending with its cancelled conclusion kept for diagnosis", check)
		}
	}
}
