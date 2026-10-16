package readmodel

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// TestProjectRunRecordsBlockerObservedAt: the persisted facts behind a run's
// blockers carry when they were observed (#5404), the latest one wins, and an
// unstamped event leaves the time unknown rather than zero.
func TestProjectRunRecordsBlockerObservedAt(t *testing.T) {
	identity := testIdentity()
	events := []journal.Event{
		ev(1, 0, journal.EventRunStarted, nil),
		ev(2, time.Minute, journal.EventError, func(e *journal.Event) {
			e.Error = &journal.ErrorDetail{Code: journal.ErrorCodeRecoveryObservationFailed, Message: "first"}
		}),
		ev(3, 2*time.Minute, journal.EventGateEvaluated, func(e *journal.Event) { e.Gate, e.Verdict = "review", "needs-changes" }),
		ev(4, 3*time.Minute, journal.EventError, func(e *journal.Event) {
			e.Error = &journal.ErrorDetail{Code: journal.ErrorCodeRecoveryObservationFailed, Message: "second"}
		}),
	}
	facts := ProjectRun(identity, Projection{}, events).Run.Operator
	if facts.LatestErrorAt == nil || !facts.LatestErrorAt.Equal(projectBase.Add(3*time.Minute)) ||
		facts.LatestError.Message != "second" {
		t.Fatalf("latest error = %+v at %v", facts.LatestError, facts.LatestErrorAt)
	}
	if facts.ReviewAt == nil || !facts.ReviewAt.Equal(projectBase.Add(2*time.Minute)) {
		t.Fatalf("review at = %v", facts.ReviewAt)
	}

	unstamped := append(append([]journal.Event(nil), events...), ev(5, 4*time.Minute, journal.EventError, func(e *journal.Event) {
		e.Error = &journal.ErrorDetail{Code: "x", Message: "unstamped"}
		e.Time = time.Time{}
	}))
	if got := ProjectRun(identity, Projection{}, unstamped).Run.Operator.LatestErrorAt; got != nil {
		t.Fatalf("unstamped error observed at %v, want unknown", got)
	}
}

// TestOperatorFactsWithoutObservedAtStillLoad: a row persisted before
// projection version 10 has no LatestErrorAt in operator_json. It must load
// with the time unknown, not fail and not read as the zero time.
func TestOperatorFactsWithoutObservedAtStillLoad(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	identity := testIdentity()
	projection := ProjectRun(identity, Projection{}, []journal.Event{
		ev(1, 0, journal.EventRunStarted, nil),
		ev(2, time.Minute, journal.EventError, func(e *journal.Event) {
			e.Error = &journal.ErrorDetail{Code: journal.ErrorCodeRecoveryObservationFailed, Message: "full"}
		}),
	})
	if err := store.UpsertRun(ctx, projection); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writer.ExecContext(ctx,
		`UPDATE run SET operator_json = ?, projection_version = ? WHERE run_id = ?`,
		`{"LatestError":{"code":"recovery_observation_failed","message":"full"}}`, currentProjectionVersion-1, identity.RunID); err != nil {
		t.Fatal(err)
	}
	row, ok, err := store.GetRun(ctx, identity.RunID)
	if err != nil || !ok {
		t.Fatalf("legacy row: ok=%v err=%v", ok, err)
	}
	if row.Operator.LatestError == nil || row.Operator.LatestError.Code != journal.ErrorCodeRecoveryObservationFailed || row.Operator.LatestErrorAt != nil {
		t.Fatalf("legacy operator facts = %+v", row.Operator)
	}
}
