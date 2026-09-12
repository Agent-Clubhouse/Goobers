package readmodel

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestCountOutcomeVerdictHonorsStartedAtWindow(t *testing.T) {
	store, err := Open(t.TempDir() + "/read.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	for index, run := range []RunRow{
		{RunID: "old", OutcomeVerdict: "merged", StartedAt: now.Add(-48 * time.Hour)},
		{RunID: "recent", OutcomeVerdict: "merged", StartedAt: now.Add(-time.Hour)},
		{RunID: "other", OutcomeVerdict: "rejected", StartedAt: now.Add(-time.Hour)},
	} {
		run.Gaggle = "example"
		run.Workflow = "merge-review"
		run.Phase = journal.PhaseCompleted
		run.Terminal = true
		run.LastActivity = run.StartedAt
		run.LastSeq = uint64(index + 1)
		if err := store.UpsertRun(context.Background(), Projection{Run: run}); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := store.CountOutcomeVerdict(context.Background(), "merged", time.Time{}); err != nil || count != 2 {
		t.Fatalf("lifetime count = %d, %v; want 2", count, err)
	}
	if count, err := store.CountOutcomeVerdict(context.Background(), "merged", now.Add(-24*time.Hour)); err != nil || count != 1 {
		t.Fatalf("window count = %d, %v; want 1", count, err)
	}
}
