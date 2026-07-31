//go:build livegiteawrite

// Live WRITE-path smoke test for the Gitea provider against a real Gitea repo.
// Not part of CI, and gated behind its own build tag AND env var so it can
// never run by accident. It exercises mutating provider methods and cleans up
// after itself. It never merges into the base branch.
//
// Prereq: a throwaway head branch (with a diff vs. base) must already exist —
// created out-of-band by the caller. Run:
//
//	GITEA_BASE_URL=https://gitea.example.com GITEA_TOKEN=... \
//	GITEA_LIVE_WRITE_REPO=owner/name \
//	GITEA_LIVE_WRITE_HEAD=goobers-smoke/write-path-check \
//	  go test ./providers/ -tags livegiteawrite -run TestLiveGiteaWrite -v -count=1
package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func liveWriteEnv(t *testing.T) (*GiteaProvider, RepositoryRef, string) {
	t.Helper()
	base := os.Getenv("GITEA_BASE_URL")
	token := os.Getenv("GITEA_TOKEN")
	target := os.Getenv("GITEA_LIVE_WRITE_REPO")
	head := os.Getenv("GITEA_LIVE_WRITE_HEAD")
	if base == "" || token == "" || target == "" || head == "" {
		t.Skip("set GITEA_BASE_URL, GITEA_TOKEN, GITEA_LIVE_WRITE_REPO, GITEA_LIVE_WRITE_HEAD to run")
	}
	owner, name, ok := strings.Cut(target, "/")
	if !ok {
		t.Fatalf("GITEA_LIVE_WRITE_REPO must be owner/name, got %q", target)
	}
	p := NewGiteaProvider(base, token)
	return p, RepositoryRef{Provider: ProviderGitea, Owner: owner, Name: name}, head
}

// TestLiveGiteaWritePullRequest: open -> poll -> list -> files -> compare -> close.
// It closes the PR (never merges), leaving base untouched.
func TestLiveGiteaWritePullRequest(t *testing.T) {
	p, repo, head := liveWriteEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pr, err := p.OpenPullRequest(ctx, PullRequestRequest{
		Repository: repo,
		Title:      "[goobers-smoke] provider write-path validation — safe to close",
		Body:       "Automated Gitea provider write-path test. Do not merge; will be closed automatically.",
		Head:       head,
		Base:       "main",
		Draft:      true,
	})
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	t.Logf("OpenPullRequest -> #%d %s", pr.Number, pr.URL)

	// Always close, even if a later assertion fails.
	defer func() {
		res, cerr := p.ClosePullRequest(context.Background(), ClosePullRequestRequest{
			Repository: repo, PullID: pr.ID, Comment: "[goobers-smoke] closing test PR",
		})
		if cerr != nil {
			t.Errorf("ClosePullRequest: %v (MANUAL CLEANUP NEEDED: PR #%d)", cerr, pr.Number)
			return
		}
		if res.Merged {
			t.Errorf("ClosePullRequest reported merged=true — the test must NOT merge into base")
		}
		t.Logf("ClosePullRequest -> closed #%d (merged=%v)", res.Number, res.Merged)
	}()

	poll, err := p.PollPullRequest(ctx, PullRequestPollRequest{Repository: repo, PullID: pr.ID})
	if err != nil {
		t.Fatalf("PollPullRequest: %v", err)
	}
	t.Logf("PollPullRequest -> state=%s mergeable=%v reviewDecision=%q", poll.State, poll.Mergeable, poll.ReviewDecision)

	prs, err := p.ListPullRequests(ctx, ListPullRequestsRequest{Repository: repo, Base: "main"})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	found := false
	for _, s := range prs {
		if s.Number == pr.Number {
			found = true
		}
	}
	if !found {
		t.Errorf("ListPullRequests did not include the just-opened PR #%d", pr.Number)
	}
	t.Logf("ListPullRequests -> %d open, contains ours=%v", len(prs), found)

	files, err := p.PullRequestFiles(ctx, repo, pr.ID)
	if err != nil {
		t.Fatalf("PullRequestFiles: %v", err)
	}
	if len(files) == 0 {
		t.Errorf("PullRequestFiles returned 0 files; expected the smoke-test artifact")
	}
	t.Logf("PullRequestFiles -> %d changed", len(files))
}

// TestLiveGiteaWriteWorkItem: create -> get -> update -> status -> claim ->
// release -> close. Operates only on the issue it creates.
func TestLiveGiteaWriteWorkItem(t *testing.T) {
	p, repo, _ := liveWriteEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	item, err := p.CreateWorkItem(ctx, CreateWorkItemRequest{
		Repository: repo,
		Title:      "[goobers-smoke] provider write-path validation — safe to close",
		Body:       "Automated Gitea provider write-path test issue. Will be closed automatically.",
	})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	t.Logf("CreateWorkItem -> #%s", item.ID)

	// Always close the created issue. Goobers drives terminal closeout with
	// WorkItemStatusDone (see cmd/goobers/issuecloseout.go) — NOT Closed, which
	// both this provider and the GitHub provider treat as a label-only status.
	// Assert the issue actually reaches the native "closed" state.
	defer func() {
		bg := context.Background()
		if _, cerr := p.UpdateWorkItemStatus(bg, UpdateWorkItemStatusRequest{
			Repository: repo, ID: item.ID, Status: WorkItemStatusDone, Comment: "[goobers-smoke] closing test issue",
		}); cerr != nil {
			t.Errorf("close issue: %v (MANUAL CLEANUP NEEDED: issue #%s)", cerr, item.ID)
			return
		}
		closed, gerr := p.GetWorkItem(bg, repo, item.ID)
		if gerr != nil {
			t.Errorf("verify close: %v (MANUAL CLEANUP NEEDED: issue #%s)", gerr, item.ID)
			return
		}
		if closed.State != "closed" {
			t.Errorf("issue #%s still %q after Done — MANUAL CLEANUP NEEDED", item.ID, closed.State)
			return
		}
		t.Logf("closed test issue #%s (state=%s)", item.ID, closed.State)
	}()

	got, err := p.GetWorkItem(ctx, repo, item.ID)
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if got.ID != item.ID {
		t.Errorf("GetWorkItem ID=%q want %q", got.ID, item.ID)
	}

	newBody := "Edited by the write-path smoke test."
	upd, err := p.UpdateWorkItem(ctx, UpdateWorkItemRequest{Repository: repo, ID: item.ID, Body: &newBody})
	if err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	t.Logf("UpdateWorkItem -> body now %q", upd.Body)

	claim, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: item.ID, RunID: "goobers-smoke-run-1"})
	if err != nil {
		t.Fatalf("ClaimWorkItem: %v", err)
	}
	if !claim.Claimed {
		t.Errorf("ClaimWorkItem: expected to win the claim, got claimed=false claimedBy=%q", claim.ClaimedBy)
	}
	t.Logf("ClaimWorkItem -> claimed=%v by=%s", claim.Claimed, claim.ClaimedBy)

	if _, err := p.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: repo, ID: item.ID, RunID: "goobers-smoke-run-1"}); err != nil {
		t.Fatalf("ReleaseWorkItemClaim: %v", err)
	}
	t.Logf("ReleaseWorkItemClaim -> ok")
}
