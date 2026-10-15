package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestGitHubPublishPullRequestStatusPostsCommitStatusOnPinnedHead proves the
// GitHub surface the merge-review verdict status uses (#4199): a commit status
// on the pinned reviewed head (never the live head, which may have moved), under
// genre/name, with GitHub's 140-character description bound applied rather
// than a 422 over prose.
func TestGitHubPublishPullRequestStatusPostsCommitStatusOnPinnedHead(t *testing.T) {
	var path string
	var body map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/9", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a pinned status must not read the live head")
	})
	mux.HandleFunc("/repos/acme/app/statuses/", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		path = r.URL.Path
		decodeJSON(t, r, &body)
		writeJSON(t, w, map[string]interface{}{"id": 42})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	long := "needs-changes for base b4se: " + strings.Repeat("é", 200)
	result, err := provider.PublishPullRequestStatus(context.Background(), PullRequestStatusRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
		Genre: MergeReviewStatusGenre, Name: MergeReviewStatusName, State: CheckStateFailing,
		Description: long, TargetURL: "https://github.com/acme/app/pull/9#issuecomment-1", HeadSHA: "reviewed-head",
	})
	if err != nil {
		t.Fatalf("PublishPullRequestStatus: %v", err)
	}
	if path != "/repos/acme/app/statuses/reviewed-head" {
		t.Fatalf("status path = %q, want the pinned reviewed head", path)
	}
	if body["context"] != MergeReviewStatusContext || body["state"] != "failure" ||
		body["target_url"] != "https://github.com/acme/app/pull/9#issuecomment-1" {
		t.Fatalf("status body = %#v", body)
	}
	description, _ := body["description"].(string)
	if utf8.RuneCountInString(description) != githubStatusDescriptionLimit || !strings.HasPrefix(description, "needs-changes for base b4se: ") {
		t.Fatalf("description = %q (%d runes), want the bounded prefix", description, utf8.RuneCountInString(description))
	}
	if result.ID != 42 {
		t.Fatalf("result = %+v", result)
	}
	if !provider.Capabilities().Has(CapPRStatusPublish) {
		t.Fatal("GitHub must declare pr.status.publish now that it implements it")
	}
}

func TestGitHubPublishPullRequestStatusStateMapping(t *testing.T) {
	for state, want := range map[CheckState]string{
		CheckStatePassing: "success", CheckStateFailing: "failure", CheckStatePending: "pending",
	} {
		if got := githubStatusState(state); got != want {
			t.Errorf("githubStatusState(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestGitHubPublishPullRequestStatusResolvesLiveHeadWhenUnpinned(t *testing.T) {
	var path string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"number": 9, "state": "open", "head": map[string]interface{}{"sha": "live-head"}})
	})
	mux.HandleFunc("/repos/acme/app/statuses/", func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeJSON(t, w, map[string]interface{}{"id": 1})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	if _, err := provider.PublishPullRequestStatus(context.Background(), PullRequestStatusRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", Name: "merge-review", State: CheckStatePassing,
	}); err != nil {
		t.Fatalf("PublishPullRequestStatus: %v", err)
	}
	if path != "/repos/acme/app/statuses/live-head" {
		t.Fatalf("status path = %q, want the live head", path)
	}
}

// TestIsStatusPublishDeniedError separates a credential that may not write
// statuses (the operator must grant it) from a failure worth retrying.
func TestIsStatusPublishDeniedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"fine-grained PAT 403", &providerResponseError{statusCode: http.StatusForbidden, body: `{"message":"Resource not accessible by personal access token"}`}, true},
		{"app 403", &providerResponseError{statusCode: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`}, true},
		{"hidden 404", &providerResponseError{statusCode: http.StatusNotFound}, true},
		{"rate-limited 403", &providerResponseError{statusCode: http.StatusForbidden, rateLimitRemaining: "0"}, false},
		{"server error", &providerResponseError{statusCode: http.StatusBadGateway}, false},
		{"untyped", errors.New("dial tcp: refused"), false},
	}
	for _, tt := range tests {
		if got := IsStatusPublishDeniedError(tt.err); got != tt.want {
			t.Errorf("%s: IsStatusPublishDeniedError = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestGitHubPollKeepsMergeReviewVerdictOutOfCI is the #4199 provider half of
// the #4188 shape: ordinary CI is green and the merge-review verdict status is
// failure. The poll must report CI passing (so the CI-remediation lane never
// treats the verdict as a CI failure) while surfacing the verdict separately
// for merge-pr, and the CI-failure feed must not list it.
func TestGitHubPollKeepsMergeReviewVerdictOutOfCI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"number": 9, "state": "open", "head": map[string]interface{}{"sha": "head"},
			"base": map[string]interface{}{"sha": "base", "ref": "main"},
		})
	})
	mux.HandleFunc("/repos/acme/app/pulls/9/reviews", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []interface{}{})
	})
	mux.HandleFunc("/repos/acme/app/issues/9/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []interface{}{})
	})
	mux.HandleFunc("/repos/acme/app/commits/head/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"state": "failure", "statuses": []map[string]interface{}{
			{"context": "ci", "state": "success"},
			{"context": MergeReviewStatusContext, "state": "failure", "description": "needs-changes for base base: fix it", "target_url": "https://verdict"},
		}})
	})
	mux.HandleFunc("/repos/acme/app/commits/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"check_runs": []map[string]interface{}{
			{"name": "make-ci", "status": "completed", "conclusion": "success"},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}

	poll, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: repo, PullID: "9"})
	if err != nil {
		t.Fatalf("PollPullRequest: %v", err)
	}
	assertVerdictKeptOutOfCI(t, poll)
	failures, err := provider.CIFailures(context.Background(), repo, "head")
	if err != nil {
		t.Fatalf("CIFailures: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("CI failures = %+v, want none: the verdict is not CI", failures)
	}
}

func TestGiteaPollKeepsMergeReviewVerdictOutOfCI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"number": 9, "state": "open", "head": map[string]interface{}{"sha": "head"},
			"base": map[string]interface{}{"sha": "base", "ref": "main"},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/9/reviews", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []interface{}{})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/9/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []interface{}{})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/commits/head/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"state": "failure", "statuses": []map[string]interface{}{
			{"context": "ci", "status": "success"},
			{"context": MergeReviewStatusContext, "status": "failure", "description": "needs-changes for base base: fix it", "target_url": "https://verdict"},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewGiteaProvider(server.URL, "token")
	repo := RepositoryRef{Owner: "acme", Name: "app"}

	poll, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: repo, PullID: "9"})
	if err != nil {
		t.Fatalf("PollPullRequest: %v", err)
	}
	assertVerdictKeptOutOfCI(t, poll)
	failures, err := provider.CIFailures(context.Background(), repo, "head")
	if err != nil {
		t.Fatalf("CIFailures: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("CI failures = %+v, want none: the verdict is not CI", failures)
	}
}

func assertVerdictKeptOutOfCI(t *testing.T, poll PullRequestPollResult) {
	t.Helper()
	if poll.CheckState != CheckStatePassing {
		t.Fatalf("CheckState = %q, want passing: the verdict status is not CI", poll.CheckState)
	}
	for _, check := range poll.Checks {
		if check.Name == MergeReviewStatusContext {
			t.Fatalf("Checks = %+v, want the verdict status kept out", poll.Checks)
		}
	}
	want := CheckDetail{
		Name: MergeReviewStatusContext, State: CheckStateFailing, Conclusion: "failure",
		URL: "https://verdict", Summary: "needs-changes for base base: fix it",
	}
	if poll.VerdictStatus == nil || *poll.VerdictStatus != want {
		t.Fatalf("VerdictStatus = %+v, want %+v", poll.VerdictStatus, want)
	}
}

// TestGitHubRefCheckStatesDoesNotReadAPendingVerdictAsCI: the GraphQL rollup
// counts the verdict status, so a pending verdict (a defer, or a withdrawn
// verdict) over green CI reads as PENDING. That ref is re-read without the
// verdict; a pending rollup with no pending verdict stays one request.
func TestGitHubRefCheckStatesDoesNotReadAPendingVerdictAsCI(t *testing.T) {
	server, calls := refCheckStatesServer(t, map[string]interface{}{
		"r0": map[string]interface{}{
			"statusCheckRollup": map[string]string{"state": "PENDING"},
			"status":            map[string]interface{}{"context": map[string]string{"state": "PENDING"}},
		},
		"r1": map[string]interface{}{
			"statusCheckRollup": map[string]string{"state": "PENDING"},
			"status":            map[string]interface{}{"context": nil},
		},
	}, map[string][]map[string]interface{}{
		"aaa111": {checkRunFixture(1, "test", actionsAppID, "completed", "success")},
	})
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	states, err := provider.RefCheckStates(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, []string{"aaa111", "bbb222"})
	if err != nil {
		t.Fatalf("RefCheckStates: %v", err)
	}
	if states["aaa111"] != CheckStatePassing || states["bbb222"] != CheckStatePending {
		t.Fatalf("states = %v, want green CI under a pending verdict passing and real pending CI pending", states)
	}
	if calls["/repos/acme/app/commits/bbb222/check-runs"] != 0 {
		t.Fatalf("requests = %v: a pending rollup without a pending verdict must not be re-read", calls)
	}
}

func TestGitHubMergeReviewStatusReadsTheStandingVerdict(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/commits/head/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"statuses": []map[string]interface{}{
			{"context": "ci", "state": "success"},
			{"context": MergeReviewStatusContext, "state": "pending", "description": "defer for base b: wait"},
		}})
	})
	mux.HandleFunc("/repos/acme/app/commits/bare/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"statuses": []map[string]interface{}{{"context": "ci", "state": "success"}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}

	got, err := provider.MergeReviewStatus(context.Background(), repo, "head")
	if err != nil || got == nil || got.State != CheckStatePending || got.Summary != "defer for base b: wait" {
		t.Fatalf("MergeReviewStatus = %+v, %v", got, err)
	}
	if got, err := provider.MergeReviewStatus(context.Background(), repo, "bare"); err != nil || got != nil {
		t.Fatalf("MergeReviewStatus(bare) = %+v, %v; want none", got, err)
	}
}

func TestIsStatusLimitError(t *testing.T) {
	limit := &providerResponseError{statusCode: http.StatusUnprocessableEntity, body: `{"errors":[{"message":"This SHA and context has reached the maximum number of statuses."}]}`}
	other := &providerResponseError{statusCode: http.StatusUnprocessableEntity, body: `{"message":"No commit found for SHA: x"}`}
	if !IsStatusLimitError(limit) || IsStatusLimitError(other) {
		t.Fatal("only the per-context status cap is a limit error")
	}
}
