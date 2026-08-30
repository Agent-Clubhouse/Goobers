package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func blockedLedgerRepo() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
}

func blockedLedgerRecords(repo providers.RepositoryRef, itemID string, blockers ...string) map[string]blockedRecord {
	return map[string]blockedRecord{
		blockedRecordKey(repo, itemID): {
			Repository: repo,
			ItemID:     itemID,
			Blockers:   blockers,
			RunID:      "recorded-run",
			Reason:     "BLOCKED_BY_DEPENDENCIES",
			RecordedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		},
	}
}

// #1911: a curation pass resolved an item's dependencies from the prose of an
// old escalation comment instead of from the recorded block, concluded
// "unblocked", and cleared the label while the recorded blocker was still
// open. The recorded blockers decide, and every one of them must be closed.
func TestStaleBlockedOnSiblingMarkerHonoursTheRecordedBlockLedger(t *testing.T) {
	repo := blockedLedgerRepo()

	t.Run("open recorded blocker keeps the marker even when the comment payload resolved", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(459, "parked item", blockedOnSiblingLabel)
		server.addIssue(458, "recorded blocker, still open")
		server.addIssue(1183, "cited in prose")
		server.addComment(459, blockedOnSiblingCommentFor(t, 1183))
		server.closeIssue(1183)
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "459", State: "open", Labels: []string{blockedOnSiblingLabel}}

		stale, err := staleBlockedOnSiblingMarker(
			context.Background(), provider, repo, item, blockedLedgerRecords(repo, "459", "458"),
		)
		if err != nil {
			t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — the recorded blocker #458 is still open")
		}
	})

	t.Run("resolved recorded blockers clear the marker without a comment payload", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(460, "parked item", blockedOnSiblingLabel)
		server.addIssue(461, "recorded blocker")
		server.closeIssue(461)
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "460", State: "open", Labels: []string{blockedOnSiblingLabel}}

		stale, err := staleBlockedOnSiblingMarker(
			context.Background(), provider, repo, item, blockedLedgerRecords(repo, "460", "461"),
		)
		if err != nil {
			t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
		}
		if !stale {
			t.Fatal("stale = false, want true — the ledger names blockers and every one is closed")
		}
	})

	t.Run("a record for another item is not this item's evidence", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(462, "parked item", blockedOnSiblingLabel)
		server.addIssue(463, "someone else's blocker")
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "462", State: "open", Labels: []string{blockedOnSiblingLabel}}

		stale, err := staleBlockedOnSiblingMarker(
			context.Background(), provider, repo, item, blockedLedgerRecords(repo, "999", "463"),
		)
		if err != nil {
			t.Fatalf("staleBlockedOnSiblingMarker: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — no recorded block and no comment payload is not proof")
		}
	})
}

func TestRecordedLedgerBlockersScoping(t *testing.T) {
	repo := blockedLedgerRepo()
	other := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "other-repo"}

	if got := recordedLedgerBlockers(blockedLedgerRecords(repo, "459", "458"), repo, "459"); len(got) != 1 || got[0] != "458" {
		t.Fatalf("recordedLedgerBlockers = %v, want [458]", got)
	}
	if got := recordedLedgerBlockers(blockedLedgerRecords(other, "459", "458"), repo, "459"); got != nil {
		t.Fatalf("recordedLedgerBlockers = %v, want nil — the record belongs to another repository", got)
	}
	unscoped := map[string]blockedRecord{"459": {ItemID: "459", Blockers: []string{"458"}}}
	if got := recordedLedgerBlockers(unscoped, repo, "459"); got != nil {
		t.Fatalf("recordedLedgerBlockers = %v, want nil — an unscoped legacy record stays quarantined", got)
	}
}

// The drifted state itself: the label was removed while the ledger still
// records an open blocker, so the operator-facing signal says ready for an
// item selection keeps excluding. Reconciliation restores the label.
func TestReconcileRestoresBlockedOnSiblingLabelDriftedFromTheLedger(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	repo := blockedLedgerRepo()
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(459, "Drifted item", "goobers:approved", providers.LabelReady)
	server.addIssue(458, "Recorded blocker, still open", "goobers:approved")
	server.addIssue(460, "Ledger self-healed", "goobers:approved", providers.LabelReady)
	server.addIssue(461, "Closed blocker", "goobers:approved")
	server.closeIssue(461)

	recs := blockedLedgerRecords(repo, "459", "458")
	for key, rec := range blockedLedgerRecords(repo, "460", "461") {
		recs[key] = rec
	}
	if err := saveBlockedRecords(blockedRecordsPath(layoutFor(root)), recs); err != nil {
		t.Fatalf("seed blocked records: %v", err)
	}

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	provider := server.newGitHubProvider("token")
	reconciled, err := reconcileBacklogMetadata(
		context.Background(), layoutFor(root), provider, repo,
		"goobers:approved", defaultBacklogStalenessPolicy(), func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciliations = %d, want 1 — only the drifted item needs correcting", reconciled)
	}

	assertFakeIssueLabels(t, server, 459, []string{blockedOnSiblingLabel}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 460, []string{providers.LabelReady}, []string{blockedOnSiblingLabel})

	server.mu.Lock()
	comments := append([]string(nil), server.issues[459].comments...)
	server.mu.Unlock()
	if len(comments) == 0 || !strings.Contains(comments[len(comments)-1], "restored `goobers:blocked-on-sibling`") {
		t.Fatalf("issue 459 comments = %q, want an explanation naming the restored label", comments)
	}
	if !strings.Contains(comments[len(comments)-1], "#458") {
		t.Fatalf("issue 459 comments = %q, want the still-open recorded blocker named", comments)
	}
}

// The restoration is a drift repair, not a second park disposition: a
// needs-human item (what reconcileBlockedCycleLabels applies to the members of
// a circular dependency) and a closed item both keep their labels as they are.
func TestDriftedBlockedOnSiblingBlockersLeavesStrongerDispositionsAlone(t *testing.T) {
	repo := blockedLedgerRepo()
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(470, "cycle member", providers.LabelNeedsHuman)
	server.addIssue(471, "recorded blocker, still open")
	server.addIssue(472, "closed item")
	provider := server.newGitHubProvider("token")

	for _, tt := range []struct {
		name string
		item providers.WorkItem
	}{
		{
			name: "needs-human",
			item: providers.WorkItem{ID: "470", State: "open", Labels: []string{providers.LabelNeedsHuman}},
		},
		{
			name: "closed item",
			item: providers.WorkItem{ID: "472", State: "closed"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			blockers, err := driftedBlockedOnSiblingBlockers(
				context.Background(), provider, repo, tt.item, blockedLedgerRecords(repo, tt.item.ID, "471"),
			)
			if err != nil {
				t.Fatalf("driftedBlockedOnSiblingBlockers: %v", err)
			}
			if len(blockers) != 0 {
				t.Fatalf("blockers = %v, want none — the marker must not be restored here", blockers)
			}
		})
	}
}
