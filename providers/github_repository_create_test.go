package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubCreateRepositoryPreviewsUserAndOrganizationOwners(t *testing.T) {
	for _, test := range []struct {
		name     string
		owner    string
		endpoint string
	}{
		{name: "user", owner: "octocat", endpoint: "/user/repos"},
		{name: "organization", owner: "config-org", endpoint: "/orgs/config-org/repos"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var createCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer create-token" {
					t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
				}
				switch r.URL.Path {
				case "/user":
					_ = json.NewEncoder(w).Encode(map[string]string{"login": "octocat"})
				case test.endpoint:
					createCalls++
					if r.Method != http.MethodPost {
						t.Errorf("method = %s, want POST", r.Method)
					}
					var body struct {
						Name       string `json:"name"`
						Visibility string `json:"visibility"`
						Private    bool   `json:"private"`
						AutoInit   bool   `json:"auto_init"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode create body: %v", err)
						http.Error(w, "invalid body", http.StatusBadRequest)
						return
					}
					if body.Name != "fleet-config" || body.Visibility != "private" ||
						!body.Private || body.AutoInit {
						t.Errorf("create body = %+v", body)
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"owner":      map[string]string{"login": test.owner},
						"name":       "fleet-config",
						"clone_url":  "https://github.com/" + test.owner + "/fleet-config.git",
						"visibility": "private",
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			provider := NewGitHubProvider("create-token")
			provider.BaseURL = server.URL
			result, err := provider.CreateRepository(context.Background(), CreateRepositoryRequest{
				Owner:      test.owner,
				Name:       "fleet-config",
				Visibility: "private",
			})
			if err != nil {
				t.Fatalf("CreateRepository: %v", err)
			}
			if createCalls != 1 ||
				result.Repository.Owner != test.owner ||
				result.Repository.Name != "fleet-config" ||
				result.Visibility != "private" {
				t.Fatalf("CreateRepository result = %+v, calls = %d", result, createCalls)
			}
		})
	}
}

func TestGitHubCreateRepositoryRejectsInvalidVisibilityBeforeRequest(t *testing.T) {
	provider := NewGitHubProvider("create-token")
	if _, err := provider.CreateRepository(context.Background(), CreateRepositoryRequest{
		Owner:      "octocat",
		Name:       "fleet-config",
		Visibility: "secret",
	}); err == nil {
		t.Fatal("CreateRepository accepted invalid visibility")
	}
}
