//go:build integration

package providers_contract

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

// TestContract_GitHubLiveSmoke is an opt-in read-only smoke test against the real
// GitHub API. It runs only when GOOBERS_GITHUB_LIVE_SMOKE=1 and a token + repo are
// provided.
func TestContract_GitHubLiveSmoke(t *testing.T) {
	if os.Getenv("GOOBERS_GITHUB_LIVE_SMOKE") != "1" {
		t.Skip("set GOOBERS_GITHUB_LIVE_SMOKE=1 (plus token + repo) to run the live smoke test")
	}
	token := firstNonEmpty(os.Getenv("GOOBERS_GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		t.Skip("live smoke test needs GOOBERS_GITHUB_TOKEN or GITHUB_TOKEN")
	}
	repoSpec := os.Getenv("GOOBERS_GITHUB_SMOKE_REPO") // "owner/name"
	owner, name, ok := strings.Cut(repoSpec, "/")
	if !ok {
		t.Skip("live smoke test needs GOOBERS_GITHUB_SMOKE_REPO in owner/name form")
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
