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
	// #5404: an unreadable shared inventory and a failure specific to the
	// observing run are journaled under different codes, because only the
	// first can be shown cleared by another run's later success.
	inventoryFailure := errors.New("recovery inventory is full")
	runFailure := errors.New("read own retained record")
	for _, tc := range []struct {
		observed error
		cause    error
		code     string
	}{
		{&RecoveryInventoryError{Err: inventoryFailure}, inventoryFailure, journal.ErrorCodeRecoveryObservationFailed},
		{runFailure, runFailure, journal.ErrorCodeRecoveryObservationRunFailed},
	} {
		r := &Runner{cfg: Config{RecoveryEvents: func(context.Context, string) ([]journal.Event, int, error) {
			return nil, 500, tc.observed
		}}}
		err := r.recordRecoveryAfterCleanup(t.Context(), nil, "run", nil)
		if !errors.Is(err, tc.cause) || workspaceCleanupErrorDetail(err).Code != tc.code {
			t.Fatalf("post-cleanup observation = %v / %+v, want code %s", err, workspaceCleanupErrorDetail(err), tc.code)
		}
	}
	r := &Runner{cfg: Config{RecoveryEvents: func(context.Context, string) ([]journal.Event, int, error) {
		return nil, 500, nil
	}}}
	removeFailure := errors.New("remove worktree")
	err := r.recordRecoveryAfterCleanup(t.Context(), nil, "run", removeFailure)
	if !errors.Is(err, removeFailure) || workspaceCleanupErrorDetail(err).Code != "worktree_remove_failed" {
		t.Fatalf("removal failure = %v / %+v", err, workspaceCleanupErrorDetail(err))
	}
}

// TestRecoveryObservationOutcomeIsReported: a later success is what retracts
// a recovery observation blocker (#5404), so the runner must report successes
// as well as failures — and nothing for a cleanup that failed before any
// observation was attempted. The reported time is when the observation
// STARTED, so a success that read the inventory before a concurrent failure
// cannot be stamped after it.
func TestRecoveryObservationOutcomeIsReported(t *testing.T) {
	type outcome struct {
		runID      string
		observedAt time.Time
		err        error
	}
	var reported []outcome
	var readStarted []time.Time
	observationFailure := errors.New("inventory unavailable")
	var observeErr error
	r := &Runner{cfg: Config{
		RecoveryEvents: func(context.Context, string) ([]journal.Event, int, error) {
			readStarted = append(readStarted, time.Now())
			return nil, 500, observeErr
		},
		RecoveryObserved: func(runID string, observedAt time.Time, err error) {
			reported = append(reported, outcome{runID, observedAt, err})
		},
	}}
	observeErr = observationFailure
	if err := r.recordRecoveryAfterCleanup(t.Context(), nil, "first", nil); !errors.Is(err, observationFailure) {
		t.Fatalf("failed observation = %v", err)
	}
	observeErr = nil
	if err := r.recordRecoveryAfterCleanup(t.Context(), nil, "second", nil); err != nil {
		t.Fatalf("successful observation = %v", err)
	}
	if err := r.recordRecoveryAfterCleanup(t.Context(), nil, "third", errors.New("remove worktree")); err == nil {
		t.Fatal("removal failure was swallowed")
	}
	if len(reported) != 2 || reported[0].runID != "first" || !errors.Is(reported[0].err, observationFailure) ||
		reported[1].runID != "second" || reported[1].err != nil {
		t.Fatalf("reported outcomes = %+v", reported)
	}
	for i, got := range reported {
		if got.observedAt.IsZero() || got.observedAt.After(readStarted[i]) {
			t.Fatalf("outcome %d observed at %v, want a time taken before the inventory read at %v", i, got.observedAt, readStarted[i])
		}
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
	if last.Error == nil || last.Error.Code != journal.ErrorCodeRecoveryObservationRunFailed {
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
