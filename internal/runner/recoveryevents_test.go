package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestRecoveryObservationsHonorConfiguredCapacityAcrossJournalBatches(t *testing.T) {
	const runID = "recovery-configured-capacity"
	log, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	events := make([]journal.Event, 129)
	for i := range events {
		events[i] = journal.Event{RunID: runID, Type: journal.EventRunnerAnnotation, Runner: map[string]any{
			"operation": "recovery-retained", "recoveryRef": fmt.Sprintf("refs/goobers/recovery/%s/%03d", runID, i),
			"recoveryRepositoryKey": "github|||team|repo|", "recoveryRetainUntil": "2026-10-08T00:00:00Z",
		}}
	}
	r := &Runner{cfg: Config{RecoveryEvents: func(context.Context, string) ([]journal.Event, int, error) {
		return events, 500, nil
	}}}
	if err := r.recordRecoveryEvents(t.Context(), log, runID); err != nil {
		t.Fatalf("129 observations below configured cap 500: %v", err)
	}
	reader, err := journal.OpenReadOnly(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Events()
	if err != nil || countRecoveryObservations(got) != 129 {
		t.Fatalf("observations=%d err=%v, want 129", countRecoveryObservations(got), err)
	}

	r.cfg.RecoveryEvents = func(context.Context, string) ([]journal.Event, int, error) { return events, 128, nil }
	if err := r.recordRecoveryEvents(t.Context(), log, runID); err == nil {
		t.Fatal("observations above configured capacity were accepted")
	}
}

func TestRecoveryObservationFailureIsNotWorktreeRemovalFailure(t *testing.T) {
	observationFailure := errors.New("inventory unavailable")
	r := &Runner{cfg: Config{RecoveryEvents: func(context.Context, string) ([]journal.Event, int, error) {
		return nil, 500, observationFailure
	}}}
	err := r.recordRecoveryAfterCleanup(t.Context(), nil, "run", nil)
	if !errors.Is(err, observationFailure) || workspaceCleanupErrorDetail(err).Code != recoveryObservationFailureCode {
		t.Fatalf("post-cleanup observation = %v / %+v", err, workspaceCleanupErrorDetail(err))
	}
	removeFailure := errors.New("remove worktree")
	err = r.recordRecoveryAfterCleanup(t.Context(), nil, "run", removeFailure)
	if !errors.Is(err, removeFailure) || workspaceCleanupErrorDetail(err).Code != "worktree_remove_failed" {
		t.Fatalf("removal failure = %v / %+v", err, workspaceCleanupErrorDetail(err))
	}
}

func TestTaskCleanupJournalsRecoveryObservationFailureSeparately(t *testing.T) {
	log, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "observation-journal", Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	done := make(chan error)
	close(done)
	err = completeTaskDispatch(log, stageHeartbeat{stop: make(chan struct{}), done: done}, "implement", 1, journal.AttemptPolicy, nil, func(bool) error {
		return &recoveryObservationError{err: errors.New("inventory unavailable")}
	})
	if err != nil {
		t.Fatalf("nonfatal observation warning changed task outcome: %v", err)
	}
	reader, err := journal.OpenReadOnly(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Error == nil || last.Error.Code != recoveryObservationFailureCode {
		t.Fatalf("cleanup journal event = %+v", last)
	}
}

func TestRecoveryObservationsUseActiveWriterAndDeduplicate(t *testing.T) {
	root := t.TempDir()
	const runID = "recovery-observation"
	log, err := journal.Create(root, journal.RunIdentity{RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	event := journal.Event{RunID: runID, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"operation": "recovery-retained", "recoveryRef": "refs/goobers/recovery/" + runID, "recoveryRepositoryKey": "github|||team|repo|", "recoveryRetainUntil": "2026-10-08T00:00:00Z"}}
	r := &Runner{cfg: Config{RecoveryEvents: func(context.Context, string) ([]journal.Event, int, error) { return []journal.Event{event}, 500, nil }}}
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

func TestEmittedRecoveryLookalikeCannotSuppressHostObservation(t *testing.T) {
	const runID = "recovery-provenance"
	log, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	emitted := journal.Event{RunID: runID, Type: journal.EventRunnerAnnotation, Runner: map[string]any{
		"operation": "recovery-retained", "recoveryRef": "refs/goobers/recovery/" + runID,
		"recoveryRepositoryKey": "github|||team|repo|", "recoveryRetainUntil": "2026-10-08T00:00:00Z",
		livejournal.EmitKeyRunnerField: "stage-controlled-key",
	}}
	if err := log.Append(emitted); err != nil {
		t.Fatal(err)
	}
	delete(emitted.Runner, livejournal.EmitKeyRunnerField)
	r := &Runner{cfg: Config{RecoveryEvents: func(context.Context, string) ([]journal.Event, int, error) { return []journal.Event{emitted}, 500, nil }}}
	if err := r.recordRecoveryEvents(context.Background(), log, runID); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	hostObservations := 0
	for _, event := range events {
		if recoveryObservationKey(event) != "" {
			hostObservations++
		}
	}
	if hostObservations != 1 || countRecoveryObservations(events) != 2 {
		t.Fatalf("stage emission suppressed host acknowledgement: host=%d total=%d", hostObservations, countRecoveryObservations(events))
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
