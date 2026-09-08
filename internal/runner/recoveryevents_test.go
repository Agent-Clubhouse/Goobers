package runner

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRecoveryObservationsUseActiveWriterAndDeduplicate(t *testing.T) {
	root := t.TempDir()
	const runID = "recovery-observation"
	log, err := journal.Create(root, journal.RunIdentity{RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	event := journal.Event{RunID: runID, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"operation": "recovery-retained", "recoveryRef": "refs/goobers/recovery/" + runID, "recoveryRepositoryKey": "github|||team|repo|", "recoveryRetainUntil": "2026-10-08T00:00:00Z"}}
	r := &Runner{cfg: Config{RecoveryEvents: func(context.Context, string) ([]journal.Event, error) { return []journal.Event{event}, nil }}}
	for range 2 {
		if err := r.recordRecoveryEvents(context.Background(), log, runID); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := journal.OpenReadOnly(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || countRecoveryObservations(events) != 1 {
		t.Fatalf("active writer publication duplicated observations: %d %v", len(events), err)
	}
	event.Runner["recoveryRetainUntil"] = "2026-11-08T00:00:00Z"
	if err := r.recordRecoveryEvents(context.Background(), log, runID); err != nil {
		t.Fatal(err)
	}
	events, err = reader.Events()
	if err != nil || countRecoveryObservations(events) != 2 {
		t.Fatalf("renewal observation was suppressed: %d %v", len(events), err)
	}
	event.RunID = "another-run"
	if err := r.recordRecoveryEvents(context.Background(), log, runID); err == nil {
		t.Fatal("cross-run observation accepted")
	}
	event.RunID = runID
	for _, branch := range []int{1, 2, 1, 2} {
		if err := r.recordRecoveryEvents(context.Background(), &branchJournal{run: log, branch: branch}, runID); err != nil {
			t.Fatal(err)
		}
	}
	events, err = reader.Events()
	if err != nil || countRecoveryObservations(events) != 4 {
		t.Fatalf("branch-local deduplication lost containment: %d %v", countRecoveryObservations(events), err)
	}
}

func countRecoveryObservations(events []journal.Event) int {
	count := 0
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["operation"] == "recovery-retained" {
			count++
		}
	}
	return count
}
