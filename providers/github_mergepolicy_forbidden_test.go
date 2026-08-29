package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A 403 from the entitlement-gated rules endpoint (private repo on a free
// plan) must degrade to direct-merge, not surface as an error: GitHub stays
// the enforcer at merge time, and classing it as an auth failure would latch
// the scheduler's auth circuit for the whole workflow.
func TestDetectMergePolicyForbiddenRulesDegradesToDirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/rules/branches/") {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Upgrade to GitHub Pro or make this repository public to enable this feature."})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	p := NewGitHubProvider("test-token")
	p.BaseURL = server.URL
	got, err := p.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{
		Repository: RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "web"},
		Branch:     "main",
	})
	if err != nil {
		t.Fatalf("DetectMergePolicy: %v", err)
	}
	if got.Policy != MergePolicyDirect {
		t.Fatalf("policy = %q, want direct", got.Policy)
	}
}
