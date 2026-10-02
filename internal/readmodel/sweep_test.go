package readmodel

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestProjectedRunIDsBeforeOrdersByTimestampAndRunID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	seedSweepRun(t, store, "run-b", base.Add(time.Minute))
	seedSweepRun(t, store, "run-oldest", base)
	seedSweepRun(t, store, "run-a", base.Add(time.Minute))
	seedSweepRun(t, store, "run-after-bound", base.Add(3*time.Minute))

	got, err := store.ProjectedRunIDsBefore(ctx, base.Add(2*time.Minute), 3)
	if err != nil {
		t.Fatalf("projected runs before: %v", err)
	}
	assertSweepRows(t, got, []RunRow{
		{RunID: "run-oldest", StartedAt: base},
		{RunID: "run-a", StartedAt: base.Add(time.Minute)},
		{RunID: "run-b", StartedAt: base.Add(time.Minute)},
	})
}

func TestProjectedRunIDsAfterUsesKeysetCursorAndDefaultLimit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	for i := 0; i < defaultListLimit+2; i++ {
		seedSweepRun(t, store, fmt.Sprintf("run-%02d", i), base.Add(time.Duration(i/3)*time.Minute))
	}

	got, err := store.ProjectedRunIDsAfter(
		ctx,
		base.Add(10*time.Minute),
		"run-31",
		base.Add(20*time.Minute),
		0,
	)
	if err != nil {
		t.Fatalf("projected runs after: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("len(projected runs after) = %d, want 20", len(got))
	}
	if got[0].RunID != "run-32" || got[0].StartedAt != base.Add(10*time.Minute) {
		t.Errorf("first row = %+v, want run-32 at cursor timestamp", got[0])
	}
	if got[len(got)-1].RunID != "run-51" || got[len(got)-1].StartedAt != base.Add(17*time.Minute) {
		t.Errorf("last row = %+v, want run-51 at final timestamp", got[len(got)-1])
	}

	all, err := store.ProjectedRunIDsAfter(ctx, base.Add(30*time.Minute), "", base.Add(20*time.Minute), 0)
	if err != nil {
		t.Fatalf("projected runs without cursor: %v", err)
	}
	if len(all) != defaultListLimit {
		t.Fatalf("default page length = %d, want %d", len(all), defaultListLimit)
	}
	if all[0].RunID != "run-00" || all[defaultListLimit-1].RunID != "run-49" {
		t.Errorf("default page bounds = %q..%q, want run-00..run-49",
			all[0].RunID, all[defaultListLimit-1].RunID)
	}
}

func seedSweepRun(t *testing.T, store *Store, runID string, startedAt time.Time) {
	t.Helper()
	err := store.UpsertRun(context.Background(), Projection{Run: RunRow{
		RunID:        runID,
		Gaggle:       "gaggle",
		Workflow:     "workflow",
		Phase:        journal.PhaseRunning,
		StartedAt:    startedAt,
		LastActivity: startedAt,
		LastSeq:      1,
	}})
	if err != nil {
		t.Fatalf("seed %s: %v", runID, err)
	}
}

func assertSweepRows(t *testing.T, got, want []RunRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(rows) = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].RunID != want[i].RunID || !got[i].StartedAt.Equal(want[i].StartedAt) {
			t.Errorf("row %d = {%q, %s}, want {%q, %s}",
				i, got[i].RunID, got[i].StartedAt, want[i].RunID, want[i].StartedAt)
		}
	}
}
