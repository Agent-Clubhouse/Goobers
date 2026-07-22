//go:build integration

package providers_contract

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/testdep"
	"github.com/goobers/goobers/providers"
)

// TestIntegrationContractGitHubLiveSmoke is an opt-in read-only smoke test
// against the real GitHub API. It runs only when GOOBERS_GITHUB_LIVE_SMOKE=1
// and a token + repo are provided.
func TestIntegrationContractGitHubLiveSmoke(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_GITHUB_LIVE_SMOKE")
	testdep.RequireEnv(t, "GOOBERS_GITHUB_TOKEN", "GITHUB_TOKEN")
	testdep.RequireEnv(t, "GOOBERS_GITHUB_SMOKE_REPO")

	token := firstNonEmpty(os.Getenv("GOOBERS_GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN"))
	repoSpec := os.Getenv("GOOBERS_GITHUB_SMOKE_REPO") // "owner/name"
	owner, name, ok := strings.Cut(repoSpec, "/")
	if !ok {
		t.Fatalf("GOOBERS_GITHUB_SMOKE_REPO = %q, want owner/name form", repoSpec)
	}
	p := providers.NewGitHubProvider(token)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items, err := p.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: providers.RepositoryRef{Owner: owner, Name: name},
		State:      "open", Limit: 50,
	})
	if err != nil {
		t.Fatalf("live ListWorkItems: %v", err)
	}
	// Every returned item must be an issue, never a pull request (PRs are excluded).
	for _, item := range items {
		if item.Type != "issue" {
			t.Fatalf("live query returned a non-issue item: %#v", item)
		}
	}
	t.Logf("live smoke ok: %s/%s returned %d open issue(s)", owner, name, len(items))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
