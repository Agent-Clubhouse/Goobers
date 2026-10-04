package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestWorkspaceRevisionProductionIdentityWiring(t *testing.T) {
	t.Setenv("REVISION_TEST_TOKEN", "configured-token")
	for _, kind := range []apiv1.Provider{apiv1.ProviderGitHub, apiv1.ProviderGitea, apiv1.ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			var root string
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests++
				wantPath := "/forge/api/v3/repos/org/repo"
				payload := map[string]interface{}{"id": 17, "name": "repo", "owner": map[string]string{"login": "org"}, "html_url": root + "/org/repo"}
				if kind == apiv1.ProviderGitea {
					wantPath = "/forge/api/v1/repos/org/repo"
				}
				if kind == apiv1.ProviderADO {
					wantPath = "/forge/org/project/_apis/git/repositories/repo"
					payload = map[string]interface{}{"id": "17", "name": "repo", "project": map[string]string{"name": "project", "id": "project-id"}, "remoteUrl": root + "/org/project/_git/repo"}
					if _, password, ok := req.BasicAuth(); !ok || password != "configured-token" {
						t.Error("ADO lookup did not use its configured credential")
					}
				} else if !strings.Contains(req.Header.Get("Authorization"), "configured-token") {
					t.Error("lookup did not use its configured credential")
				}
				if req.URL.Path != wantPath || req.Method != http.MethodGet {
					t.Errorf("lookup route = %s %s", req.Method, req.URL.Path)
				}
				if err := json.NewEncoder(w).Encode(payload); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			root = server.URL + "/forge"
			repo := instance.RepoRef{Provider: string(kind), BaseURL: root, Owner: "org", Name: "repo", Token: instance.TokenRef{Env: "REVISION_TEST_TOKEN"}}
			if kind == apiv1.ProviderADO {
				repo.Project = "project"
			}
			cfg := &instance.Config{Repos: []instance.RepoRef{repo}}
			reg := journal.NewRegistryScrubber()
			resolver, _, err := buildCredentials(cfg, nil, repo.Owner, repo.Name, nil, reg)
			if err != nil {
				t.Fatal(err)
			}
			lookup := buildRevisionIdentityResolver(cfg, resolver, reg, nil)
			configured := apiv1.RepoRef{Provider: kind, BaseURL: root, Owner: "org", Project: repo.Project, Name: "repo"}
			revision := apiv1.WorkspaceRevision{Repository: apiv1.RepositoryIdentity{Provider: kind, Owner: "org", Project: repo.Project, Name: "repo", ID: "17", URL: root}, CommitSHA: strings.Repeat("a", 40)}
			if _, err := workspacerevision.Resolve(context.Background(), revision, configured, nil, lookup); err != nil {
				t.Fatal(err)
			}
			revision.Repository.URL = "https://attacker.invalid/forged"
			if _, err := workspacerevision.Resolve(context.Background(), revision, configured, nil, lookup); err == nil {
				t.Fatal("forged stage URL authorized")
			}
			if requests != 1 {
				t.Fatalf("configured route reads=%d, want 1; forged hosts must not trigger lookups", requests)
			}
		})
	}
}
