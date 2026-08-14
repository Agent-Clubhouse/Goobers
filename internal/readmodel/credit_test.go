package readmodel

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestCreditAssignmentRanksSharedNodesAcrossRuns(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	seedCreditRun(t, store, "failed-a", start, journal.PhaseFailed, []StageRow{
		{RunID: "failed-a", Stage: "shared-review", Attempts: 2},
		{RunID: "failed-a", Stage: "implement", Attempts: 1},
	})
	seedCreditRun(t, store, "escalated-b", start.Add(time.Hour), journal.PhaseEscalated, []StageRow{
		{RunID: "escalated-b", Stage: "shared-review", Attempts: 3},
	})
	seedCreditRun(t, store, "completed-c", start.Add(2*time.Hour), journal.PhaseCompleted, []StageRow{
		{RunID: "completed-c", Stage: "shared-review", Attempts: 1},
	})

	got, err := store.CreditAssignment(ctx, CreditOptions{
		Gaggle: "core",
		Since:  start.Add(-time.Minute),
		Until:  start.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d ranked nodes, want 2: %+v", len(got), got)
	}
	if got[0] != (NodeCredit{
		Gaggle: "core", Workflow: "implementation", Stage: "shared-review",
		RoutedRuns: 3, FailureRuns: 1, EscalationRuns: 1, RetryWasteAttempts: 3,
	}) {
		t.Errorf("top node = %+v", got[0])
	}
	if got[1].Stage != "implement" || got[1].FailureRuns != 1 {
		t.Errorf("second node = %+v, want failed implement node", got[1])
	}
}

func TestCreditAssignmentAppliesWindowAndWorkflowScope(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	seedCreditRun(t, store, "inside", start, journal.PhaseFailed, []StageRow{
		{RunID: "inside", Stage: "review", Attempts: 1},
	})
	seedCreditRun(t, store, "outside", start.Add(-48*time.Hour), journal.PhaseFailed, []StageRow{
		{RunID: "outside", Stage: "review", Attempts: 1},
	})

	got, err := store.CreditAssignment(ctx, CreditOptions{
		Gaggle: "core", Workflow: "implementation", Since: start.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RoutedRuns != 1 {
		t.Fatalf("scoped credit = %+v, want one routed run", got)
	}
}

func seedCreditRun(
	t *testing.T,
	store *Store,
	runID string,
	startedAt time.Time,
	phase journal.RunPhase,
	stages []StageRow,
) {
	t.Helper()
	finishedAt := startedAt.Add(time.Minute)
	if err := store.UpsertRun(context.Background(), Projection{
		Run: RunRow{
			RunID: runID, Gaggle: "core", Workflow: "implementation",
			Phase: phase, Terminal: true, StartedAt: startedAt,
			FinishedAt: &finishedAt, LastActivity: finishedAt, LastSeq: 1,
		},
		Stages: stages,
	}); err != nil {
		t.Fatalf("seed %s: %v", runID, err)
	}
}
