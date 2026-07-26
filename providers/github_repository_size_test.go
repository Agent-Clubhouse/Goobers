package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubProviderRepositorySizeKB(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"size": 2097152})
	}))
	defer server.Close()

	provider := NewGitHubProvider("size-token")
	provider.BaseURL = server.URL
	size, err := provider.RepositorySizeKB(context.Background(), RepositoryRef{
		Provider: ProviderGitHub,
		Owner:    "acme",
		Name:     "monorepo",
	})
	if err != nil {
		t.Fatalf("RepositorySizeKB: %v", err)
	}
	if size != 2097152 {
		t.Fatalf("size = %d, want 2097152", size)
	}
	if gotPath != "/repos/acme/monorepo" {
		t.Fatalf("request path = %q, want /repos/acme/monorepo", gotPath)
	}
	if gotAuth != "Bearer size-token" {
		t.Fatalf("Authorization header = %q, want Bearer size-token", gotAuth)
	}
}

func TestGitHubProviderRepositorySizeKBPropagatesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	provider := NewGitHubProvider("size-token")
	provider.BaseURL = server.URL
	if _, err := provider.RepositorySizeKB(context.Background(), RepositoryRef{
		Provider: ProviderGitHub,
		Owner:    "acme",
		Name:     "missing",
	}); err == nil {
		t.Fatal("RepositorySizeKB accepted a 404 response without error")
	}
}
