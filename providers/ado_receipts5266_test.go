package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// adoPRServer stands up the find-or-create pull-request endpoints, returning
// whether a PR already exists so both OpenPullRequest paths can be driven.
func adoPRServer(t *testing.T, failCreate bool) (*httptest.Server, *bool) {
	t.Helper()
	created := false
	pr := map[string]interface{}{
		"pullRequestId": 42,
		"url":           "api-pr-url",
		"_links":        map[string]interface{}{"web": map[string]string{"href": "https://ado.example/pr/42"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			value := []interface{}{}
			if created {
				existing := map[string]interface{}{
					"pullRequestId": 42,
					"url":           "api-pr-url",
					"sourceRefName": "refs/heads/goobers/implementation/run-1",
					"targetRefName": "refs/heads/main",
					"_links":        map[string]interface{}{"web": map[string]string{"href": "https://ado.example/pr/42"}},
				}
				value = append(value, existing)
			}
			writeJSON(t, w, map[string]interface{}{"value": value})
		case http.MethodPost:
			if failCreate {
				http.Error(w, `{"message":"source branch does not exist"}`, http.StatusBadRequest)
				return
			}
			created = true
			writeJSON(t, w, pr)
		default:
			t.Fatalf("unexpected pullrequests method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		writeJSON(t, w, pr)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &created
}

// TestADOOpenPullRequestRecordsCreateAndUpdateEffects is #5266's core
// acceptance: a successful ADO PR creation or update yields a navigable receipt
// exactly once per recorded effect path.
//
// Before this, OpenPullRequest returned on both paths without recording
// anything, so Work Items — which lists RECORDED provider effects — had no
// receipt at all for a PR that demonstrably existed. The page was not wrong
// about what it showed; the effect was never published to it.
func TestADOOpenPullRequestRecordsCreateAndUpdateEffects(t *testing.T) {
	server, _ := adoPRServer(t, false)
	recorder := &adoMutationRecorder{}
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	provider.SetMutationRecorder(recorder)
	repo := RepositoryRef{Name: "repo", Project: "project"}

	if _, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: repo, Title: "t", Body: "b",
		Head: "refs/heads/goobers/implementation/run-1", Base: "refs/heads/main",
	}); err != nil {
		t.Fatalf("OpenPullRequest (create): %v", err)
	}
	if len(recorder.refs) != 1 {
		t.Fatalf("create recorded %d effects, want exactly 1: %+v", len(recorder.refs), recorder.refs)
	}
	create := recorder.refs[0]
	if create.Provider != ProviderADO || create.Operation != "create" || create.Ref != "ado#42" {
		t.Errorf("create receipt = %+v", create)
	}
	// Navigable: the whole point of the receipt is that an operator can reach
	// the entity, and a repository-scoped _git path is what ADO serves a PR on.
	wantURL := server.URL + "/org/project/_git/repo/pullrequest/42"
	if create.URL != wantURL {
		t.Errorf("create receipt URL = %q, want %q", create.URL, wantURL)
	}

	// Second call finds the existing PR and PATCHes it: a distinct recorded
	// effect path, which must also yield exactly one receipt.
	if _, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: repo, Title: "t2", Body: "b2",
		Head: "goobers/implementation/run-1", Base: "main",
	}); err != nil {
		t.Fatalf("OpenPullRequest (update): %v", err)
	}
	if len(recorder.refs) != 2 {
		t.Fatalf("update recorded %d total effects, want 2: %+v", len(recorder.refs), recorder.refs)
	}
	if update := recorder.refs[1]; update.Operation != "update" || update.URL != wantURL {
		t.Errorf("update receipt = %+v", update)
	}
}

// TestADOOpenPullRequestFailureRecordsNoEffect is the other half of "exactly
// once per recorded effect path": #5266 requires that failed or conflicting
// operations do NOT count as confirmed mutations. A receipt is evidence that
// something happened, so recording one for a rejected request would make Work
// Items assert a mutation the forge refused.
func TestADOOpenPullRequestFailureRecordsNoEffect(t *testing.T) {
	server, _ := adoPRServer(t, true)
	recorder := &adoMutationRecorder{}
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	provider.SetMutationRecorder(recorder)

	if _, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Title:      "t", Body: "b", Head: "refs/heads/h", Base: "refs/heads/main",
	}); err == nil {
		t.Fatal("OpenPullRequest succeeded against a rejecting server")
	}
	if len(recorder.refs) != 0 {
		t.Fatalf("a refused creation recorded %d effects, want 0: %+v", len(recorder.refs), recorder.refs)
	}
}

// TestADOEntityWebURLScoping pins the constructed identity, including the cases
// that must stay unknown. #5266 requires that a historical unknown identity
// remain explicitly unknown, so an absent org/project/repository yields "" —
// never a URL with empty path segments, which would render as a link that 404s
// while looking resolved.
func TestADOEntityWebURLScoping(t *testing.T) {
	for _, tc := range []struct {
		name         string
		organization string
		project      string
		repo         RepositoryRef
		kind         string
		id           string
		want         string
	}{
		{
			name: "work item is project scoped", organization: "org", project: "proj",
			repo: RepositoryRef{Name: "repo", Project: "proj"}, kind: "issue", id: "7",
			want: "https://dev.azure.com/org/proj/_workitems/edit/7",
		},
		{
			// PR numbering is per repository, so project alone cannot identify
			// one; the _git segment carries the repository.
			name: "pull request is repository scoped", organization: "org", project: "proj",
			repo: RepositoryRef{Name: "repo", Project: "proj"}, kind: "pr", id: "42",
			want: "https://dev.azure.com/org/proj/_git/repo/pullrequest/42",
		},
		{
			name:         "repository ref project overrides the provider default",
			organization: "org", project: "default", repo: RepositoryRef{Name: "repo", Project: "other"},
			kind: "issue", id: "7",
			want: "https://dev.azure.com/org/other/_workitems/edit/7",
		},
		{
			name: "no organization stays unknown", organization: "", project: "proj",
			repo: RepositoryRef{Name: "repo"}, kind: "issue", id: "7", want: "",
		},
		{
			name: "no project stays unknown", organization: "org", project: "",
			repo: RepositoryRef{Name: "repo"}, kind: "issue", id: "7", want: "",
		},
		{
			name:         "pull request without a repository stays unknown",
			organization: "org", project: "proj", repo: RepositoryRef{Project: "proj"},
			kind: "pr", id: "42", want: "",
		},
		{
			name: "no id stays unknown", organization: "org", project: "proj",
			repo: RepositoryRef{Name: "repo"}, kind: "issue", id: "", want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewADOProvider(tc.organization, tc.project, "token")
			if got := p.entityWebURL(tc.repo, tc.kind, tc.id); got != tc.want {
				t.Errorf("entityWebURL = %q, want %q", got, tc.want)
			}
		})
	}
}
