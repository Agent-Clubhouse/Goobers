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

// blockerClear is one blocker.cleared record as cmd/goobers' recovery
// observation recorder writes it: observedAt is when the successful
// observation started, writtenAt when the record reached the journal.
type blockerClear struct {
	code, scope, runID    string
	observedAt, writtenAt time.Time
	omitObservedAt        bool
}

func instanceClear(at time.Time) blockerClear {
	return blockerClear{code: journal.ErrorCodeRecoveryObservationFailed, scope: "instance", runID: "observer", observedAt: at, writtenAt: at}
}

func appendBlockerCleared(t *testing.T, layout instance.Layout, clears ...blockerClear) {
	t.Helper()
	for _, clear := range clears {
		appendOneBlockerCleared(t, layout, clear)
	}
}

func appendOneBlockerCleared(t *testing.T, layout instance.Layout, clear blockerClear) {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time { return clear.writtenAt }))
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"kind": journal.RunnerAnnotationBlockerCleared, "code": clear.code, "scope": clear.scope}
	if !clear.omitObservedAt {
		payload["observedAt"] = clear.observedAt.Format(time.RFC3339Nano)
	}
	if err := log.Append(journal.Event{Type: journal.EventRunnerAnnotation, RunID: clear.runID, Runner: payload}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func listStatusBlockers(t *testing.T, service *Local, runID string) []OperatorBlocker {
	t.Helper()
	runs, err := service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return blockerFor(t, runs, runID)
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
	appendBlockerCleared(t, layout, instanceClear(observedAt.Add(-time.Second)))
	runs, err = service.ListStatusRuns(context.Background(), StatusRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blockers := blockerFor(t, runs, "observation-failed"); len(blockers) != 1 {
		t.Fatalf("an earlier clear retracted a later failure: %+v", blockers)
	}

	appendBlockerCleared(t, layout, instanceClear(startedAt.Add(2*time.Hour)))
	unregistered := instanceClear(startedAt.Add(2 * time.Hour))
	unregistered.code = "worktree_remove_failed"
	appendBlockerCleared(t, layout, unregistered)
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

// TestBlockerClearUsesObservationStartNotWriteTime: a success that read the
// inventory BEFORE a concurrent failure, but whose record reached the journal
// AFTER it, must not retract that failure — the condition may still be
// present. A record carrying no observation time clears nothing.
func TestBlockerClearUsesObservationStartNotWriteTime(t *testing.T) {
	service, layout, _ := fixtureService(t)
	startedAt := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	createErroredFixtureRun(t, layout, "concurrent-failure", startedAt, journal.ErrorCodeRecoveryObservationFailed)
	failedAt := startedAt.Add(time.Minute)
	service.now = func() time.Time { return startedAt.Add(time.Hour) }

	late := instanceClear(failedAt.Add(-time.Second))
	late.writtenAt = failedAt.Add(time.Minute)
	appendBlockerCleared(t, layout, late)
	if blockers := listStatusBlockers(t, service, "concurrent-failure"); len(blockers) != 1 {
		t.Fatalf("a success observed before the failure but written after it retracted it: %+v", blockers)
	}

	undated := instanceClear(failedAt.Add(time.Hour))
	undated.omitObservedAt = true
	appendBlockerCleared(t, layout, undated)
	if blockers := listStatusBlockers(t, service, "concurrent-failure"); len(blockers) != 1 {
		t.Fatalf("a clear without an observation time retracted a failure: %+v", blockers)
	}

	appendBlockerCleared(t, layout, instanceClear(failedAt.Add(time.Second)))
	if blockers := listStatusBlockers(t, service, "concurrent-failure"); len(blockers) != 0 {
		t.Fatalf("a success observed after the failure did not retract it: %+v", blockers)
	}
}

// TestRunScopedObservationFailureIsClearedOnlyBySameRun: when the inventory
// was readable but the run's own records, checks or journal append failed,
// another run's success — and an instance-level clear — proves nothing about
// it. Only a later successful observation of the same run retracts it.
func TestRunScopedObservationFailureIsClearedOnlyBySameRun(t *testing.T) {
	service, layout, _ := fixtureService(t)
	startedAt := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	createErroredFixtureRun(t, layout, "own-record-failed", startedAt, journal.ErrorCodeRecoveryObservationRunFailed)
	after := startedAt.Add(2 * time.Minute)
	service.now = func() time.Time { return startedAt.Add(time.Hour) }

	asInstance := instanceClear(after)
	asInstance.code = journal.ErrorCodeRecoveryObservationRunFailed
	appendBlockerCleared(t, layout, instanceClear(after), asInstance,
		blockerClear{code: journal.ErrorCodeRecoveryObservationRunFailed, scope: "run", runID: "another-run", observedAt: after, writtenAt: after})
	blockers := listStatusBlockers(t, service, "own-record-failed")
	if len(blockers) != 1 || blockers[0].Code != journal.ErrorCodeRecoveryObservationRunFailed {
		t.Fatalf("another run's success retracted a run-specific failure: %+v", blockers)
	}

	appendBlockerCleared(t, layout, blockerClear{code: journal.ErrorCodeRecoveryObservationRunFailed, scope: "run", runID: "own-record-failed", observedAt: after, writtenAt: after})
	if blockers := listStatusBlockers(t, service, "own-record-failed"); len(blockers) != 0 {
		t.Fatalf("the same run's later success did not retract its failure: %+v", blockers)
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

	appendBlockerCleared(t, layout, instanceClear(startedAt.Add(30*time.Minute)))
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
