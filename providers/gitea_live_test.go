//go:build livegitea

// Live smoke test for the Gitea provider against a real Gitea instance.
// Not part of CI. Run explicitly (see below for the required env vars).
//
// It exercises the read paths (backlog list/get/comments, PR list) against the
// repo named by GITEA_LIVE_REPO and validates that real Gitea JSON decodes into
// the provider's model structs.
//
//	GITEA_BASE_URL=https://gitea.example.com GITEA_TOKEN=... \
//	GITEA_LIVE_REPO=owner/name \
//	  go test ./providers/ -tags livegitea -run TestLiveGitea -v -count=1
package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func liveGiteaProvider(t *testing.T) (*GiteaProvider, RepositoryRef) {
	t.Helper()
	base := os.Getenv("GITEA_BASE_URL")
	token := os.Getenv("GITEA_TOKEN")
	owner, name, ok := strings.Cut(os.Getenv("GITEA_LIVE_REPO"), "/")
	if base == "" || token == "" || !ok || owner == "" || name == "" {
		t.Skip("set GITEA_BASE_URL, GITEA_TOKEN, and GITEA_LIVE_REPO=owner/name to run the live Gitea smoke test")
	}
	p := NewGiteaProvider(base, token)
	repo := RepositoryRef{Provider: ProviderGitea, Owner: owner, Name: name}
	return p, repo
}

func TestLiveGiteaListWorkItems(t *testing.T) {
	p, repo := liveGiteaProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	items, err := p.ListWorkItems(ctx, ListWorkItemsRequest{Repository: repo, State: "open", Limit: 50})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) == 0 {
		t.Fatalf("expected open issues on the target repo, got 0 — field mapping or pagination is wrong")
	}
	t.Logf("target repo open backlog: %d items", len(items))
	for _, it := range items {
		if it.ID == "" || it.Title == "" {
			t.Errorf("item decoded with empty ID/Title: %+v", it)
		}
		if it.Provider != ProviderGitea {
			t.Errorf("item #%s Provider=%q, want gitea", it.ID, it.Provider)
		}
		t.Logf("  #%s [%v] %s", it.ID, it.Labels, it.Title)
	}
}

func TestLiveGiteaGetWorkItemAndComments(t *testing.T) {
	p, repo := liveGiteaProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	items, err := p.ListWorkItems(ctx, ListWorkItemsRequest{Repository: repo, State: "open", Limit: 1})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) == 0 {
		t.Skip("no open items to fetch")
	}
	id := items[0].ID

	got, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		t.Fatalf("GetWorkItem(%s): %v", id, err)
	}
	if got.ID != id {
		t.Errorf("GetWorkItem returned ID %q, want %q", got.ID, id)
	}
	t.Logf("GetWorkItem #%s: state=%s labels=%v title=%q", got.ID, got.State, got.Labels, got.Title)

	comments, err := p.ListComments(ctx, repo, id)
	if err != nil {
		t.Fatalf("ListComments(%s): %v", id, err)
	}
	t.Logf("issue #%s has %d comments (decoded cleanly)", id, len(comments))
}

func TestLiveGiteaListPullRequests(t *testing.T) {
	p, repo := liveGiteaProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	prs, err := p.ListPullRequests(ctx, ListPullRequestsRequest{Repository: repo})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	t.Logf("target repo open PRs: %d (listed + decoded without error)", len(prs))
}
