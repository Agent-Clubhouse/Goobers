package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCreatePullRequestConflictNeverUpdatesExisting(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost {
					t.Errorf("create-only attempted %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message":"another PR appeared"}`))
			}))
			defer server.Close()
			var create func(context.Context, PullRequestRequest) (PullRequestResult, error)
			if kind == ProviderGitHub {
				p := NewGitHubProvider("fixture", WithMaxTransientRetries(0), func(p *GitHubProvider) { p.BaseURL = server.URL })
				create = p.CreatePullRequest
			} else {
				p := NewADOProvider("owner", "project", "fixture", WithADOMaxRateLimitRetries(0), func(p *ADOProvider) { p.BaseURL = server.URL })
				create = p.CreatePullRequest
			}
			_, err := create(t.Context(), PullRequestRequest{Repository: RepositoryRef{Provider: kind, Owner: "owner", Project: "project", Name: "repo"}, Head: "child", Base: "main", Title: "owned"})
			if err == nil || calls.Load() != 1 {
				t.Fatal("conflict must remain uncertain without another provider request", calls.Load(), err)
			}
		})
	}
}
