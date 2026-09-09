package main

import (
	"context"
	"slices"
	"testing"
	"time"

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

// #4545 asks that blocked-on-sibling resolution treat an ISSUE blocker, and a
// blocker closed by hand rather than merged, exactly like a merged PR blocker.
// Both already hold for the two records this function reads — the "all named
// blockers closed" case above closes issue blockers by hand, and
// TestStaleBlockedOnSiblingMarkerHonoursTheRecordedBlockLedger does the same
// for the ledger. What was missing was the invariant, and the boundary.

// The operator ruling (2026-08-22) that clearing a block must preserve:
// removing a stale marker states that a condition no longer holds. Deciding
// the item deserves another attempt is a separate human judgement, so the
// correction removes the marker, adds nothing, and leaves an escalation alone.
func TestClearingBlockedOnSiblingNeverRereadiesAnItem(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(20, "escalated and parked", blockedOnSiblingLabel, providers.LabelNeedsHuman, providers.LabelReady)
	server.addIssue(720, "blocker issue")
	server.addComment(20, blockedOnSiblingCommentFor(t, 720))
	// Closed by hand, not merged: no unpark path fires on a hand close, which
	// is exactly the resolution #4545 names.
	server.closeIssue(720)
	provider := server.newGitHubProvider("token")
	item, err := provider.GetWorkItem(context.Background(), repo, "20")
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}

	correction, _, err := inspectBacklogMetadata(
		context.Background(), provider, repo, item, "goobers-bot",
		time.Now().UTC(), defaultBacklogStalenessPolicy(), nil,
	)
	if err != nil {
		t.Fatalf("inspectBacklogMetadata: %v", err)
	}
	if !slices.Contains(correction.removeLabels, blockedOnSiblingLabel) {
		t.Fatalf("removeLabels = %v, want the stale block marker removed", correction.removeLabels)
	}
	if len(correction.addLabels) != 0 {
		t.Fatalf("addLabels = %v, want none — clearing a block never re-readies an item", correction.addLabels)
	}
	if slices.Contains(correction.removeLabels, providers.LabelNeedsHuman) {
		t.Fatalf("removeLabels = %v, want the escalation left in place", correction.removeLabels)
	}
}

// The ownership boundary between this pass and backlog-query's blocked
// re-sweep (#4545). An item whose blockers are recorded ONLY as native GitHub
// issue dependencies must keep its marker here, however those blockers
// resolved: appendBlockedResweepCandidates selects it into a
// `dependency-recheck` curation run, and that selection requires the label.
//
// Teaching this function to read native dependencies looks like the obvious
// completion of it, and silently breaks that lane — reconciliation runs first
// in the same `backlog-query --claim --curation` invocation, so the item loses
// its park label, drops out of the re-sweep's input, and is then claimed
// through ORDINARY eligibility with no curation review at all.
func TestReconcileLeavesNativelyBlockedItemsToTheDependencyRecheckLane(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	for _, blockerState := range []string{"open", "closed"} {
		t.Run("native blocker "+blockerState, func(t *testing.T) {
			server := newFakeGitHubServer(t, repo.Owner, repo.Name)
			server.addIssue(30, "natively blocked", blockedOnSiblingLabel)
			server.addIssue(730, "native blocker")
			server.setIssueBlockers(30, 730)
			if blockerState == "closed" {
				server.closeIssue(730)
			}
			provider := server.newGitHubProvider("token")
			item, err := provider.GetWorkItem(context.Background(), repo, "30")
			if err != nil {
				t.Fatalf("GetWorkItem: %v", err)
			}
			if item.BlockedByCount == 0 {
				t.Fatal("fixture records no native dependency, so this test proves nothing")
			}

			stale, err := staleBlockedOnSiblingMarker(context.Background(), provider, repo, item, nil)
			if err != nil {
				t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
			}
			if stale {
				t.Fatal("stale = true, want false — a natively recorded block belongs to the dependency-recheck re-sweep, not to this pass")
			}
		})
	}
}
