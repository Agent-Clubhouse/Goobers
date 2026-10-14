package readservice

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// appendBlockerCleared journals what cmd/goobers' recovery observation
// recorder writes after a successful observation, stamped at at.
func appendBlockerCleared(t *testing.T, layout instance.Layout, code string, at time.Time) {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, RunID: "observer",
		Runner: map[string]any{"kind": journal.RunnerAnnotationBlockerCleared, "code": code},
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func createErroredFixtureRun(t *testing.T, layout instance.Layout, runID string, startedAt time.Time, code string) {
	t.Helper()
	run, clock := createFixtureRun(t, layout, fixtureMachine(t), runID, "implementation", "goobers",
		startedAt, journal.Trigger{Kind: journal.TriggerManual}, false)
	clock.now = startedAt.Add(time.Minute)
	if err := run.Append(journal.Event{
		Type: journal.EventError, Stage: "implement",
		Error: &journal.ErrorDetail{Code: code, Message: "observe retained recovery state: recovery inventory is full"},
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)
}

func blockerFor(t *testing.T, runs []RunSummary, runID string) []OperatorBlocker {
	t.Helper()
	for _, run := range runs {
		if run.ID == runID {
			if len(run.Operator.Blockers) != len(run.Operator.PotentialBlockers) {
				t.Fatalf("run %s blockers %+v disagree with potentialBlockers %q", runID, run.Operator.Blockers, run.Operator.PotentialBlockers)
			}
			return run.Operator.Blockers
		}
	}
	t.Fatalf("run %s not listed in %+v", runID, runs)
	return nil
}

// TestRecoveryObservationBlockerIsRetractedByLaterObservation is #5404 at the
// read service: a recovery_observation_failed blocker carries its observed-at
// time and age, survives a clear recorded BEFORE it, and is retracted by a
// clear recorded AFTER it — on every read surface — while an unregistered
// code is never retracted by a clear record, however recent.
func TestRecoveryObservationBlockerIsRetractedByLaterObservation(t *testing.T) {
	service, layout, _ := fixtureService(t)
	startedAt := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	createErroredFixtureRun(t, layout, "observation-failed", startedAt, journal.ErrorCodeRecoveryObservationFailed)
	createErroredFixtureRun(t, layout, "removal-failed", startedAt.Add(time.Second), "worktree_remove_failed")
	observedAt := startedAt.Add(time.Minute)
	service.now = func() time.Time { return startedAt.Add(3 * time.Hour) }

	runs, err := service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	blockers := blockerFor(t, runs, "observation-failed")
	if len(blockers) != 1 || blockers[0].Code != journal.ErrorCodeRecoveryObservationFailed ||
		blockers[0].Source != BlockerSourceError ||
		blockers[0].ObservedAt == nil || !blockers[0].ObservedAt.Equal(observedAt) ||
		blockers[0].AgeMillis == nil || *blockers[0].AgeMillis != (3*time.Hour-time.Minute).Milliseconds() {
		t.Fatalf("blockers before any clear = %+v", blockers)
	}

	// A success observed before the failure proves nothing about it.
	appendBlockerCleared(t, layout, journal.ErrorCodeRecoveryObservationFailed, observedAt.Add(-time.Second))
	runs, err = service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blockers := blockerFor(t, runs, "observation-failed"); len(blockers) != 1 {
		t.Fatalf("an earlier clear retracted a later failure: %+v", blockers)
	}

	appendBlockerCleared(t, layout, journal.ErrorCodeRecoveryObservationFailed, startedAt.Add(2*time.Hour))
	appendBlockerCleared(t, layout, "worktree_remove_failed", startedAt.Add(2*time.Hour))
	runs, err = service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blockers := blockerFor(t, runs, "observation-failed"); len(blockers) != 0 {
		t.Fatalf("status blockers after a later successful observation = %+v, want none", blockers)
	}
	if blockers := blockerFor(t, runs, "removal-failed"); len(blockers) != 1 || blockers[0].Code != "worktree_remove_failed" {
		t.Fatalf("a clear record retracted an unregistered code: %+v", blockers)
	}
	list, err := service.ListRuns(context.Background(), RunListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blockers := blockerFor(t, list.Runs, "observation-failed"); len(blockers) != 0 {
		t.Fatalf("run list blockers after clear = %+v", blockers)
	}
	detail, err := service.GetRun(context.Background(), "observation-failed")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Operator.Blockers) != 0 || len(detail.Operator.PotentialBlockers) != 0 {
		t.Fatalf("run detail blockers after clear = %+v", detail.Operator)
	}
	if detail.Operator.LatestError == nil || detail.Operator.LatestError.Code != journal.ErrorCodeRecoveryObservationFailed {
		t.Fatalf("the recorded error itself must stay as history: %+v", detail.Operator.LatestError)
	}
}

// TestReadModelBlockersCarryObservedAtAndNeverClearWithoutIt covers the
// persisted path: a projected row carries LatestErrorAt and is retracted by a
// later clear, while a row projected before observed-at existed (no
// LatestErrorAt) keeps its blocker — an unknown time cannot be proven older
// than the clear.
func TestReadModelBlockersCarryObservedAtAndNeverClearWithoutIt(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	store, err := readmodel.Open(filepath.Join(t.TempDir(), readmodel.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	startedAt := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	observedAt := startedAt.Add(time.Minute)
	seed := func(runID string, at *time.Time) {
		finishedAt := startedAt.Add(2 * time.Minute)
		row := readmodel.RunRow{
			RunID: runID, Gaggle: "goobers", Workflow: "implementation",
			WorkflowVersion: 1, WorkflowDigest: "sha256:wf",
			Phase: journal.PhaseCompleted, Terminal: true,
			StartedAt: startedAt, FinishedAt: &finishedAt, LastActivity: finishedAt, LastSeq: 3,
		}
		row.Operator.LatestError = &journal.ErrorDetail{Code: journal.ErrorCodeRecoveryObservationFailed, Message: "inventory full"}
		row.Operator.LatestErrorAt = at
		if err := store.UpsertRun(context.Background(), readmodel.Projection{Run: row}); err != nil {
			t.Fatal(err)
		}
	}
	seed("projected", &observedAt)
	seed("legacy", nil)
	service, err := NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions(), ReadModel: store}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return startedAt.Add(time.Hour) }

	runs, err := service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	projected := blockerFor(t, runs, "projected")
	if len(projected) != 1 || projected[0].ObservedAt == nil || !projected[0].ObservedAt.Equal(observedAt) ||
		projected[0].AgeMillis == nil || *projected[0].AgeMillis != (59*time.Minute).Milliseconds() {
		t.Fatalf("projected blockers = %+v", projected)
	}
	if legacy := blockerFor(t, runs, "legacy"); len(legacy) != 1 || legacy[0].ObservedAt != nil || legacy[0].AgeMillis != nil {
		t.Fatalf("legacy blockers = %+v, want one with no observed time", legacy)
	}

	appendBlockerCleared(t, layout, journal.ErrorCodeRecoveryObservationFailed, startedAt.Add(30*time.Minute))
	runs, err = service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blockers := blockerFor(t, runs, "projected"); len(blockers) != 0 {
		t.Fatalf("projected blockers after clear = %+v", blockers)
	}
	if blockers := blockerFor(t, runs, "legacy"); len(blockers) != 1 {
		t.Fatalf("a blocker with no observed time was cleared: %+v", blockers)
	}
}

// TestJournalBlockersListEachReviewProblemOnce: the journal-derived summary
// used to append an unreadable-rationale blocker for EVERY review gate, so a
// run that re-reviewed listed the same problem repeatedly, and kept it after a
// later review was readable. Only the latest review counts, as in the read
// model.
func TestJournalBlockersListEachReviewProblemOnce(t *testing.T) {
	service, layout, machine := fixtureService(t)
	startedAt := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	run, clock := createFixtureRun(t, layout, machine, "re-reviewed", "implementation", "goobers",
		startedAt, journal.Trigger{Kind: journal.TriggerManual}, false)
	invalid, err := run.RecordArtifact("review-verdict.json", []byte("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		clock.now = startedAt.Add(time.Duration(i+1) * time.Minute)
		if err := run.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Ref: &invalid}); err != nil {
			t.Fatal(err)
		}
	}
	service.now = func() time.Time { return startedAt.Add(time.Hour) }
	runs, err := service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	blockers := blockerFor(t, runs, "re-reviewed")
	if len(blockers) != 1 || blockers[0].Code != BlockerCodeReviewEvidenceUnreadable ||
		blockers[0].ObservedAt == nil || !blockers[0].ObservedAt.Equal(startedAt.Add(2*time.Minute)) {
		t.Fatalf("blockers after two unreadable reviews = %+v, want the latest one once", blockers)
	}

	readable, err := json.Marshal(map[string]string{"decision": "pass", "rationale": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordArtifact("review-verdict-2.json", readable)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = startedAt.Add(3 * time.Minute)
	if err := run.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Ref: &ref}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	runs, err = service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blockers := blockerFor(t, runs, "re-reviewed"); len(blockers) != 0 {
		t.Fatalf("a readable later review left %+v", blockers)
	}
}

func TestAddBlockerReplacesSameSourceAndCodeInPlace(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	first, second := now.Add(-2*time.Hour), now.Add(-time.Hour)
	var operator OperatorRunSummary
	operator.addBlocker(OperatorBlocker{Code: "a", Source: BlockerSourceError, Message: "a: old", ObservedAt: &first}, now)
	operator.addBlocker(OperatorBlocker{Code: "b", Source: BlockerSourceReview, Message: "b"}, now)
	operator.addBlocker(OperatorBlocker{Code: "a", Source: BlockerSourceError, Message: "a: new", ObservedAt: &second}, now)
	if len(operator.Blockers) != 2 || operator.Blockers[0].Message != "a: new" ||
		*operator.Blockers[0].AgeMillis != time.Hour.Milliseconds() || operator.Blockers[1].AgeMillis != nil {
		t.Fatalf("blockers = %+v", operator.Blockers)
	}
	if len(operator.PotentialBlockers) != 2 || operator.PotentialBlockers[0] != "a: new" || operator.PotentialBlockers[1] != "b" {
		t.Fatalf("potentialBlockers = %q", operator.PotentialBlockers)
	}
}
