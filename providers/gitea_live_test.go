//go:build livegitea

// Live smoke test for the Gitea provider against a real Gitea instance.
// Not part of CI. Run explicitly:
//
//	GITEA_BASE_URL=http://192.168.0.182:3000 GITEA_TOKEN=... \
//	  go test ./providers/ -tags livegitea -run TestLiveGitea -v -count=1
//
// It exercises the read paths (backlog list/get/comments, PR list) against
// gneitzke/HikeView3d and validates that real Gitea JSON decodes into the
// provider's model structs.
package providers

import (
	"context"
	"os"
	"testing"
	"time"
)

func liveGiteaProvider(t *testing.T) (*GiteaProvider, RepositoryRef) {
	t.Helper()
	base := os.Getenv("GITEA_BASE_URL")
	token := os.Getenv("GITEA_TOKEN")
	if base == "" || token == "" {
		t.Skip("set GITEA_BASE_URL and GITEA_TOKEN to run the live Gitea smoke test")
	}
	p := NewGiteaProvider(base, token)
	repo := RepositoryRef{Provider: ProviderGitea, Owner: "gneitzke", Name: "HikeView3d"}
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
		t.Fatalf("expected open issues on HikeView3d, got 0 — field mapping or pagination is wrong")
	}
	t.Logf("HikeView3d open backlog: %d items", len(items))
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
	t.Logf("HikeView3d open PRs: %d (listed + decoded without error)", len(prs))
}
