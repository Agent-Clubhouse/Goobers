package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const (
	adoTestProjectID    = "11111111-2222-3333-4444-555555555555"
	adoTestRepositoryID = "66666666-7777-8888-9999-000000000000"
)

type fakeADORepositoryIDReader struct {
	ids   providers.ADORepositoryIDs
	err   error
	calls int
}

func (f *fakeADORepositoryIDReader) RepositoryIDs(context.Context, providers.RepositoryRef) (providers.ADORepositoryIDs, error) {
	f.calls++
	return f.ids, f.err
}

// TestADOPullRequestURLByIDMatch pins #5960's run --pr fix: ADO's raw API
// URL names the project and repository by GUID, which the name-based matcher
// (ADO-N39) can never accept. Those GUIDs are now checked against the
// configured repository's own ids — a match is accepted, a mismatch stays
// "outside configured repository", and ids that cannot be read get a
// distinct error pointing at the web URL rather than the misleading generic
// one. Non-GUID URLs never trigger the extra read.
func TestADOPullRequestURLByIDMatch(t *testing.T) {
	resolved := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "platform", Name: "web"}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "platform", Name: "web"}
	configuredIDs := providers.ADORepositoryIDs{ProjectID: adoTestProjectID, RepositoryID: adoTestRepositoryID}
	byGUID := "https://dev.azure.com/acme/" + adoTestProjectID + "/_apis/git/repositories/" + adoTestRepositoryID + "/pullRequests/42"

	for _, tc := range []struct {
		name      string
		url       string
		reader    any
		wantErr   string // "" means accepted
		wantReads int
	}{
		{name: "guid form of the configured repository", url: byGUID, reader: &fakeADORepositoryIDReader{ids: configuredIDs}, wantReads: 1},
		{
			name: "upper-case guids and visualstudio host", reader: &fakeADORepositoryIDReader{ids: configuredIDs}, wantReads: 1,
			url: "https://acme.visualstudio.com/" + strings.ToUpper(adoTestProjectID) + "/_apis/git/repositories/" + strings.ToUpper(adoTestRepositoryID) + "/pullRequests/42",
		},
		{
			name: "project by name, repository by guid", reader: &fakeADORepositoryIDReader{ids: configuredIDs}, wantReads: 1,
			url: "https://dev.azure.com/acme/platform/_apis/git/repositories/" + adoTestRepositoryID + "/pullRequests/42",
		},
		{
			name: "another repository's guid", url: byGUID, wantReads: 1, wantErr: "outside configured repository",
			reader: &fakeADORepositoryIDReader{ids: providers.ADORepositoryIDs{ProjectID: adoTestProjectID, RepositoryID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}},
		},
		{
			name: "ids unreadable", url: byGUID, wantReads: 1, wantErr: "could not be checked against configured repository acme/platform/web",
			reader: &fakeADORepositoryIDReader{err: errors.New("status 403")},
		},
		{name: "provider cannot read ids", url: byGUID, reader: struct{}{}, wantErr: "open the pull request at its web URL https://dev.azure.com/acme/platform/_git/web/pullrequest/42"},
		{name: "wrong organization", reader: &fakeADORepositoryIDReader{ids: configuredIDs}, wantErr: "outside configured repository",
			url: "https://dev.azure.com/other/" + adoTestProjectID + "/_apis/git/repositories/" + adoTestRepositoryID + "/pullRequests/42"},
		{name: "wrong pull request number", reader: &fakeADORepositoryIDReader{ids: configuredIDs}, wantErr: "outside configured repository",
			url: "https://dev.azure.com/acme/" + adoTestProjectID + "/_apis/git/repositories/" + adoTestRepositoryID + "/pullRequests/7"},
		{name: "api form naming another repository by name", reader: &fakeADORepositoryIDReader{ids: configuredIDs}, wantErr: "outside configured repository",
			url: "https://dev.azure.com/acme/platform/_apis/git/repositories/other/pullRequests/42"},
		{name: "browser form elsewhere", reader: &fakeADORepositoryIDReader{ids: configuredIDs}, wantErr: "outside configured repository",
			url: "https://dev.azure.com/acme/platform/_git/other/pullrequest/42"},
		{name: "empty url", reader: &fakeADORepositoryIDReader{ids: configuredIDs}, wantErr: "outside configured repository", url: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := adoPullRequestURLByIDMatch(context.Background(), tc.reader, repo, resolved, tc.url, 42, "acme/platform/web")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("adoPullRequestURLByIDMatch(%q) = %v, want accepted", tc.url, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("adoPullRequestURLByIDMatch(%q) = %v, want an error containing %q", tc.url, err, tc.wantErr)
			}
			if fake, ok := tc.reader.(*fakeADORepositoryIDReader); ok && fake.calls != tc.wantReads {
				t.Fatalf("repository id reads = %d, want %d", fake.calls, tc.wantReads)
			}
		})
	}
}

// TestADOPullRequestURLByIDMatchReadsConfiguredRepositoryIDs drives the
// check through the real ADO provider: the ids come from the configured
// repository's own GET, not from anything the pull request reported.
func TestADOPullRequestURLByIDMatchReadsConfiguredRepositoryIDs(t *testing.T) {
	var reads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acme/platform/_apis/git/repositories/web" {
			http.NotFound(w, r)
			return
		}
		reads++
		writeADOJSON(t, w, map[string]any{"id": adoTestRepositoryID, "project": map[string]any{"id": adoTestProjectID}})
	}))
	defer server.Close()
	provider := providers.NewADOProvider("acme", "platform", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
	resolved := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "platform", Name: "web"}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "platform", Name: "web"}
	rawURL := "https://dev.azure.com/acme/" + adoTestProjectID + "/_apis/git/repositories/" + adoTestRepositoryID + "/pullRequests/42"

	if err := adoPullRequestURLByIDMatch(context.Background(), provider, repo, resolved, rawURL, 42, "acme/platform/web"); err != nil {
		t.Fatalf("adoPullRequestURLByIDMatch = %v, want the configured repository's own GUIDs accepted", err)
	}
	if reads != 1 {
		t.Fatalf("repository reads = %d, want 1", reads)
	}
}

// TestValidateTargetedPullRequestVerifiesADOGUIDFormURL drives the fix
// through run --pr's real validation path (validateTargetedPullRequest), not
// just the helper: an open ADO pull request whose web link is the GUID API
// form is accepted when the GUIDs are the configured repository's own, and
// rejected as outside it when the repository GUID belongs elsewhere.
func TestValidateTargetedPullRequestVerifiesADOGUIDFormURL(t *testing.T) {
	const otherRepositoryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	apiURL := "https://dev.azure.com/acme/" + adoTestProjectID + "/_apis/git/repositories/" + adoTestRepositoryID + "/pullRequests/42"
	for _, tc := range []struct {
		name         string
		repositoryID string
		wantErr      string
	}{
		{name: "configured repository's own ids", repositoryID: adoTestRepositoryID},
		{name: "another repository's id", repositoryID: otherRepositoryID, wantErr: "outside configured repository acme/platform/web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var repositoryReads int
			mux := http.NewServeMux()
			mux.HandleFunc("/acme/platform/_apis/git/repositories/web/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
				writeADOJSON(t, w, map[string]any{
					"pullRequestId": 42,
					"status":        "active",
					"url":           apiURL,
					"_links":        map[string]any{"web": map[string]any{"href": apiURL}},
					"repository": map[string]any{
						"id": adoTestRepositoryID, "name": "web",
						"project": map[string]any{"id": adoTestProjectID, "name": "platform"},
					},
				})
			})
			mux.HandleFunc("/acme/platform/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
				writeADOJSON(t, w, map[string]any{"value": []any{}})
			})
			mux.HandleFunc("/acme/platform/_apis/git/repositories/web", func(w http.ResponseWriter, _ *http.Request) {
				repositoryReads++
				writeADOJSON(t, w, map[string]any{"id": tc.repositoryID, "project": map[string]any{"id": adoTestProjectID}})
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			previous := stageProviderFactories[providers.ProviderADO]
			t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
			stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
				return providers.NewADOProvider(cfg.repo.Owner, cfg.repo.Project, "token", func(p *providers.ADOProvider) {
					p.BaseURL = server.URL
				}), nil
			}
			cfg := &instance.Config{
				APIVersion: instance.ConfigAPIVersion,
				Kind:       instance.ConfigKind,
				Repos:      []instance.RepoRef{{Provider: "ado", Owner: "acme", Project: "platform", Name: "web"}},
			}
			err := validateTargetedPullRequest(context.Background(), t.TempDir(), cfg, nil, journal.NewRegistryScrubber(),
				localscheduler.WorkflowEntry{Workflow: "merge-review"}, 42)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateTargetedPullRequest = %v, want the configured repository's GUID-form URL accepted", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("validateTargetedPullRequest = %v, want an error containing %q", err, tc.wantErr)
			}
			if repositoryReads != 1 {
				t.Fatalf("configured repository reads = %d, want exactly 1", repositoryReads)
			}
		})
	}
}
