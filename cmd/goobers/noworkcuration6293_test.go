package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/providers"
)

// TestResweepNoWorkNeverParksReadyItem is the #6293 regression: a single-item
// curate-resweep run that keeps confirming a goobers:ready item needs no
// change must not strip its readiness or park it as goobers:needs-human.
// The implementation breaker itself stays covered by
// TestRepeatedNoWorkParksItemAtThreshold.
func TestResweepNoWorkNeverParksReadyItem(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Ready item", providers.LabelApproved, providers.LabelReady)
	const runID = "resweep-6293"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
	configureCurationResweep(t, "1", "1")
	t.Setenv("GOOBERS_WORKFLOW", "curate-resweep")
	t.Chdir(t.TempDir())
	if code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root); code != 0 {
		t.Fatalf("resweep claim: code=%d stderr=%q", code, stderr)
	}

	l := layoutFor(root)
	items, err := claimedItemsForRun(l, runID)
	if err != nil {
		t.Fatalf("claimedItemsForRun: %v", err)
	}
	if len(items) != 1 || items[0].ItemID != "7" || items[0].Purpose != itemPurposeCuration {
		t.Fatalf("resweep claim recorded %+v, want item 7 with purpose %q", items, itemPurposeCuration)
	}
	seedRunWithEvents(t, l, runID, []journal.Event{{
		Type: journal.EventStageFinished, Stage: "curate", Status: string(apiv1.ResultNoWork),
		Outputs: map[string]any{"noWorkReason": "already correctly ready; nothing to change"},
	}})

	fake := &blockedHandlerFakeCommenter{}
	for i := 0; i <= nowork.StreakThreshold; i++ {
		if err := settleNoWorkStreak(context.Background(), fake, l, runID, "curate", ""); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("resweep no-work touched the item: %+v", fake.calls)
	}
	record, err := loadNoWorkStreakRecord(context.Background(), l, items[0].Repo, "7")
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if record.Count != 0 {
		t.Fatalf("resweep no-work accrued streak %d, want 0", record.Count)
	}
}
