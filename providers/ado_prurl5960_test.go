package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// adoAPIPullRequestURL is the raw _apis form ADO returns as a pull request's
// "url": it names the project and repository by GUID.
const adoAPIPullRequestURL = "https://dev.azure.com/org/11111111-2222-3333-4444-555555555555/_apis/git/repositories/66666666-7777-8888-9999-000000000000/pullRequests/42"

// adoBarePullRequest is a pull request payload carrying neither _links.web
// nor a named repository identity (only the GUIDs policy evaluation needs) —
// the rare response in which no browser URL can be derived from what the
// server said.
func adoBarePullRequest() map[string]interface{} {
	return map[string]interface{}{
		"pullRequestId": 42,
		"status":        "active",
		"url":           adoAPIPullRequestURL,
		"sourceRefName": "refs/heads/goobers/implementation/run-1",
		"targetRefName": "refs/heads/main",
		"repository": map[string]interface{}{
			"id":      "66666666-7777-8888-9999-000000000000",
			"project": map[string]string{"id": "proj-guid"},
		},
	}
}

// TestADOPullRequestReadPathsNeverSynthesizeCallerRepositoryURL pins #5960:
// a pull request read back by PollPullRequest, GetPullRequest or
// ListPullRequests that carries neither _links.web nor a repository identity
// must surface the server's own (raw) URL, never one built from the caller's
// repository. Synthesizing the caller's repository there would assert the
// very identity run.go's fail-closed repository check is about to verify.
func TestADOPullRequestReadPathsNeverSynthesizeCallerRepositoryURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, adoBarePullRequest())
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"value": []interface{}{adoBarePullRequest()}})
	})
	mux.HandleFunc("/org/project/_apis/policy/evaluations", policyEvaluationsHandler(t, nil))
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	callerRepoURL := provider.entityWebURL(repo, "pr", "42")
	if callerRepoURL == "" {
		t.Fatal("test precondition: the caller-repository URL must be constructible, or the assertions below prove nothing")
	}

	poll, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{Repository: repo, PullID: "42"})
	if err != nil {
		t.Fatalf("PollPullRequest: %v", err)
	}
	if poll.URL != adoAPIPullRequestURL {
		t.Fatalf("PollPullRequest URL = %q, want the raw server URL %q (not the caller's %q)", poll.URL, adoAPIPullRequestURL, callerRepoURL)
	}

	got, err := provider.GetPullRequest(context.Background(), repo, "42")
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if got.URL != adoAPIPullRequestURL {
		t.Fatalf("GetPullRequest URL = %q, want the raw server URL", got.URL)
	}

	listed, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{Repository: repo, SkipCheckState: true})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(listed) != 1 || listed[0].URL != adoAPIPullRequestURL {
		t.Fatalf("ListPullRequests = %+v, want one PR with the raw server URL", listed)
	}
}

// TestADOListPullRequestsURLPrefersWebLinkThenRepositoryIdentity pins the
// List path's other two URL forms: _links.web.href when present, otherwise
// the browser URL built from the server-returned repository identity.
func TestADOListPullRequestsURLPrefersWebLinkThenRepositoryIdentity(t *testing.T) {
	withLink := adoBarePullRequest()
	withLink["pullRequestId"] = 7
	withLink["_links"] = map[string]interface{}{"web": map[string]string{"href": "https://ado.example/pr/7"}}
	withIdentity := adoBarePullRequest()
	withIdentity["pullRequestId"] = 8
	withIdentity["repository"] = map[string]interface{}{
		"id": "repo-guid", "name": "other-repo",
		"project": map[string]string{"id": "proj-guid", "name": "other-project"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"value": []interface{}{withLink, withIdentity}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	listed, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, SkipCheckState: true,
	})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("ListPullRequests = %+v, want two PRs", listed)
	}
	if listed[0].URL != "https://ado.example/pr/7" {
		t.Fatalf("web-link PR URL = %q, want the _links.web.href", listed[0].URL)
	}
	// The server's repository identity wins over the caller's: a PR that
	// lives elsewhere must read as living elsewhere.
	if want := server.URL + "/org/other-project/_git/other-repo/pullrequest/8"; listed[1].URL != want {
		t.Fatalf("repository-identity PR URL = %q, want %q", listed[1].URL, want)
	}
}

// TestADOCreatePullRequestURLFallsBackToCallerRepository pins the one place
// the caller's repository may stand in (#5960): a create (or update) was
// addressed to that repository, so a response without _links.web or a
// repository identity still yields its browser URL rather than an _apis one.
func TestADOCreatePullRequestURLFallsBackToCallerRepository(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		writeJSON(t, w, adoBarePullRequest())
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.CreatePullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Title:      "t", Body: "b", Head: "goobers/implementation/run-1", Base: "main",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if want := server.URL + "/org/project/_git/repo/pullrequest/42"; result.URL != want {
		t.Fatalf("created PR URL = %q, want the caller-repository browser URL %q", result.URL, want)
	}
}
