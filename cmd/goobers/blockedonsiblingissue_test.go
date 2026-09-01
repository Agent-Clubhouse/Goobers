package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// #3355: issues parked as blocked-on-sibling have no unpark path at all. The
// only one that exists (unparkResolvedSiblings) iterates PULL REQUESTS and
// fires only when a bot PR merges, so a blocker closed by hand never triggers
// anything. 60 open issues carried the label with no way to shed it.
func TestStaleBlockedOnSiblingMarkerForIssues(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("no label, nothing to clear", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(1, "issue 1")
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "1"}

		stale, err := staleBlockedOnSiblingMarker(context.Background(), provider, repo, item, nil)
		if err != nil {
			t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — the issue carries no marker")
		}
	})

	// THE POLARITY TEST, and the reason this function exists separately from
	// blockedOnSiblingStillBlocks. That one fails OPEN on an absent record,
	// which is right when deciding whether a PR may be SELECTED. Here the
	// decision is whether to REMOVE a label, and roughly half the parked
	// issues record their blockers as native GitHub dependencies rather than
	// as this comment payload. Failing open would strip the marker from every
	// one of those, unparking issues that are genuinely still blocked.
	t.Run("labeled with no recorded blocker set fails CLOSED", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(2, "issue 2", blockedOnSiblingLabel)
		server.addComment(2, "registered the native GitHub dependency; it will self-clear when it lands")
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "2", Labels: []string{blockedOnSiblingLabel}}

		stale, err := staleBlockedOnSiblingMarker(context.Background(), provider, repo, item, nil)
		if err != nil {
			t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — without a recorded blocker set there is no proof the block resolved")
		}
	})

	t.Run("open blocker keeps the marker", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(3, "issue 3", blockedOnSiblingLabel)
		server.addIssue(700, "blocker still open")
		server.addComment(3, blockedOnSiblingCommentFor(t, 700))
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "3", Labels: []string{blockedOnSiblingLabel}}

		stale, err := staleBlockedOnSiblingMarker(context.Background(), provider, repo, item, nil)
		if err != nil {
			t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
		}
		if stale {
			t.Fatal("stale = false expected — blocker #700 is still open")
		}
	})

	// The #3394 case: its blocker #3393 closed by hand at 05:43Z and the label
	// was still there fifteen hours later.
	t.Run("all named blockers closed clears the marker", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(4, "issue 4", blockedOnSiblingLabel)
		server.addIssue(701, "blocker one")
		server.addIssue(702, "blocker two")
		server.addComment(4, blockedOnSiblingCommentFor(t, 701, 702))
		server.closeIssue(701)
		server.closeIssue(702)
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "4", Labels: []string{blockedOnSiblingLabel}}

		stale, err := staleBlockedOnSiblingMarker(context.Background(), provider, repo, item, nil)
		if err != nil {
			t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
		}
		if !stale {
			t.Fatal("stale = false, want true — every named blocker is closed")
		}
	})

	// Partial resolution is not resolution.
	t.Run("one closed one open keeps the marker", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(5, "issue 5", blockedOnSiblingLabel)
		server.addIssue(703, "closed blocker")
		server.addIssue(704, "open blocker")
		server.addComment(5, blockedOnSiblingCommentFor(t, 703, 704))
		server.closeIssue(703)
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "5", Labels: []string{blockedOnSiblingLabel}}

		stale, err := staleBlockedOnSiblingMarker(context.Background(), provider, repo, item, nil)
		if err != nil {
			t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — #704 is still open")
		}
	})
}

// The reconcile pass must actually SELECT an issue carrying only the block
// marker. Without this the check runs on no input, which is indistinguishable
// from never having been written.
func TestReconcileSelectsIssuesCarryingOnlyTheBlockMarker(t *testing.T) {
	item := providers.WorkItem{ID: "9", Labels: []string{blockedOnSiblingLabel}}
	if !hasReconciledMetadataLabel(item) {
		t.Fatal("an issue carrying only goobers:blocked-on-sibling must be inspected — it is the one marker nothing else can clear")
	}
}
