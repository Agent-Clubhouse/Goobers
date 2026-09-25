package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadRepositoryMetadataUsesConfiguredRoute(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea, ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			var root string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := "/forge/api/v3/repos/org/repo"
				payload := map[string]interface{}{"id": 17, "name": "repo", "owner": map[string]string{"login": "org"}, "html_url": root + "/org/repo"}
				switch kind {
				case ProviderGitea:
					path = "/forge/api/v1/repos/org/repo"
				case ProviderADO:
					path = "/forge/org/project/_apis/git/repositories/configured-id"
					payload = map[string]interface{}{"id": "configured-id", "name": "repo", "project": map[string]string{"name": "project", "id": "project-id"}, "remoteUrl": root + "/org/project/_git/repo"}
				}
				if r.Method != http.MethodGet || r.URL.Path != path || r.Header.Get("Authorization") == "" {
					t.Errorf("request = %s %s, authenticated=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
				}
				if err := json.NewEncoder(w).Encode(payload); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			root = server.URL + "/forge"
			route := RepositoryRef{Provider: kind, Owner: "org", Project: "project", Name: "repo"}
			var metadata RepositoryMetadata
			var err error
			switch kind {
			case ProviderGitHub:
				p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = root + "/api/v3" })
				metadata, err = p.ReadRepository(context.Background(), route)
			case ProviderGitea:
				metadata, err = NewGiteaProvider(root, "token").ReadRepository(context.Background(), route)
			case ProviderADO:
				route.Name = "configured-id"
				p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = root })
				metadata, err = p.ReadRepository(context.Background(), route)
			}
			if err != nil {
				t.Fatal(err)
			}
			if metadata.ServiceRoot != root || metadata.Repository.Provider != kind || metadata.Repository.Owner != "org" ||
				metadata.Repository.Name != "repo" || metadata.Repository.ID == "" || metadata.Repository.URL == "" {
				t.Fatalf("incomplete identity: %+v", metadata)
			}
		})
	}
}
