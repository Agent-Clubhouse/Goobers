package readmodel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

var projectBase = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func ev(seq uint64, offset time.Duration, t journal.EventType, mutate func(*journal.Event)) journal.Event {
	e := journal.Event{
		Schema: journal.EventSchema,
		Seq:    seq,
		Time:   projectBase.Add(offset),
		Type:   t,
	}
	if mutate != nil {
		mutate(&e)
	}
	return e
}

func testIdentity() journal.RunIdentity {
	return journal.RunIdentity{
		RunID:           "run-0001",
		Workflow:        "implementation",
		WorkflowVersion: 3,
		Gaggle:          "alpha",
		Trigger:         journal.Trigger{Kind: journal.TriggerSchedule, Ref: "0 * * * *"},
		StartedAt:       projectBase,
	}
}

func TestPinnedWorkspaceQueueIsVisibleAsCurrentStage(t *testing.T) {
	queued := ev(2, time.Second, journal.EventRunnerAnnotation, func(e *journal.Event) {
		e.Runner = map[string]any{"workspaceMode": "pinned", "queuePosition": float64(3)}
	})
	acquired := ev(3, 2*time.Second, journal.EventRunnerAnnotation, func(e *journal.Event) {
		e.Runner = map[string]any{"workspaceMode": "pinned", "queuePosition": float64(0)}
	})
	run := ProjectRun(testIdentity(), Projection{}, []journal.Event{
		ev(1, 0, journal.EventRunStarted, nil), queued,
	}).Run
	if run.CurrentStage != "Workspace queue (position 3)" {
		t.Fatalf("current stage = %q, want visible queue position", run.CurrentStage)
	}
	run = ProjectRun(testIdentity(), Projection{Run: run}, []journal.Event{acquired}).Run
	if run.CurrentStage != "" {
		t.Fatalf("current stage after acquisition = %q, want cleared", run.CurrentStage)
	}
}

func TestPinnedWorkspaceResetSuggestionRemainsVisibleAfterFailure(t *testing.T) {
	suggestion := "Run `goobers workspace reset <repo>` before retrying."
	run := ProjectRun(testIdentity(), Projection{}, []journal.Event{
		ev(1, time.Second, journal.EventRunnerAnnotation, func(e *journal.Event) {
			e.Runner = map[string]any{
				"kind":          "workspace_reset_suggested",
				"workspaceMode": "pinned",
				"failureStreak": float64(3),
				"suggestion":    suggestion,
			}
		}),
		ev(2, 2*time.Second, journal.EventRunFinished, func(e *journal.Event) {
			e.Status = string(journal.PhaseFailed)
		}),
	}).Run

	want := workspaceResetSuggestionPrefix + " " + suggestion
	if run.CurrentStage != want {
		t.Fatalf("terminal current stage = %q, want portal-visible suggestion %q", run.CurrentStage, want)
	}
}

// completedRunEvents is a run that starts, retries once, and completes.
func completedRunEvents() []journal.Event {
	return []journal.Event{
		ev(1, time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		ev(2, 2*time.Second, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Status, e.AttemptClass = "implement", "failure", journal.AttemptPolicy
		}),
		ev(3, 3*time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		ev(4, 4*time.Second, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Status = "implement", "success"
		}),
		ev(5, 5*time.Second, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }),
		ev(6, 6*time.Second, journal.EventGateEvaluated, func(e *journal.Event) { e.Gate = "review" }),
		ev(7, 7*time.Second, journal.EventRunFinished, func(e *journal.Event) {
			e.Status = string(journal.PhaseCompleted)
		}),
	}
}

// TestProjectionIsIncrementalAndWholeAtOnce is §14.9's determinism property in
// the form that matters most day to day: projecting a run in one pass and
// projecting it in tail-sized pieces must produce the SAME row.
//
// This is what makes rebuild and incremental projection interchangeable. If they
// diverged, a rebuilt store would answer differently from an incrementally-built
// one — and since rebuild is the recovery path, the difference would only ever
// be discovered after an incident.
func TestProjectionIsIncrementalAndWholeAtOnce(t *testing.T) {
	identity := testIdentity()
	events := completedRunEvents()

	whole := ProjectRun(identity, Projection{}, events)

	// Now the same events, applied in every possible split point.
	for split := 0; split <= len(events); split++ {
		first := ProjectRun(identity, Projection{}, events[:split])
		second := ProjectRun(identity, first, events[split:])
		if !reflect.DeepEqual(second.Run, whole.Run) {
			t.Errorf("split at %d produced a different run row:\n incremental = %+v\n whole       = %+v",
				split, second.Run, whole.Run)
		}
		if !reflect.DeepEqual(second.Stages, whole.Stages) {
			t.Errorf("split at %d produced different stage rows:\n incremental = %+v\n whole       = %+v",
				split, second.Stages, whole.Stages)
		}
	}
}

// TestProjectionIsDeterministic pins that the same input yields byte-identical
// output across repeated runs — §14.9's "rebuilding from the same journals
// produces byte-identical canonical rows".
//
// Map iteration is the usual way this breaks, which is why stages and stage rows
// are sorted rather than emitted in map order.
func TestProjectionIsDeterministic(t *testing.T) {
	identity := testIdentity()
	events := completedRunEvents()
	first := ProjectRun(identity, Projection{}, events)
	for i := 0; i < 20; i++ {
		again := ProjectRun(identity, Projection{}, events)
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("projection %d differed from the first:\n got  = %+v\n want = %+v", i, again, first)
		}
	}
}

func TestProjectionKeepsEarliestClaimedIssueIdentity(t *testing.T) {
	identity := testIdentity()
	identity.Trigger = journal.Trigger{Kind: journal.TriggerItem, Ref: "trigger-item"}
	events := []journal.Event{
		ev(1, time.Second, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Status = "prepare", "success"
			e.Outputs = map[string]any{"title": "Preparation title"}
		}),
		ev(2, 2*time.Second, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Status = "query-backlog", "success"
			e.Outputs = map[string]any{"id": "3088", "title": "Claimed issue title"}
		}),
		ev(3, 3*time.Second, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Status = "open-pr", "success"
			e.Outputs = map[string]any{"id": "4001", "title": "Pull request title"}
		}),
		ev(4, 4*time.Second, journal.EventRefTouched, func(e *journal.Event) {
			e.ExternalRef = &journal.ExternalRef{Kind: "issue", ID: "3088", URL: "https://github.com/acme/app/issues/3088"}
		}),
		ev(5, 5*time.Second, journal.EventRefTouched, func(e *journal.Event) {
			e.ExternalRef = &journal.ExternalRef{Kind: "pr", ID: "4001", URL: "https://github.com/acme/app/pull/4001"}
		}),
	}

	whole := ProjectRun(identity, Projection{}, events)
	if got := whole.Run.Operator; got.IssueNumber != "3088" || got.IssueTitle != "Claimed issue title" {
		t.Fatalf("operator issue = #%s %q, want #3088 claimed issue title", got.IssueNumber, got.IssueTitle)
	}
	if got := whole.Run.Operator; got.IssueURL != "https://github.com/acme/app/issues/3088" ||
		got.PullRequest == nil || got.PullRequestTitle != "Pull request title" {
		t.Fatalf("operator associated work = %+v", got)
	}

	first := ProjectRun(identity, Projection{}, events[:2])
	incremental := ProjectRun(identity, first, events[2:])
	if !reflect.DeepEqual(incremental.Run.Operator, whole.Run.Operator) {
		t.Fatalf("incremental operator = %+v, whole = %+v", incremental.Run.Operator, whole.Run.Operator)
	}
}

func TestOperatorTrajectoryDefaultsActiveStagesToImplementing(t *testing.T) {
	for _, stage := range []string{"query-backlog", "gather-implement-context", "custom-active-stage"} {
		if got := OperatorTrajectory(stage, journal.PhaseRunning); got != "implementing" {
			t.Errorf("OperatorTrajectory(%q, running) = %q, want implementing", stage, got)
		}
	}
	if got := OperatorTrajectory("query-backlog", journal.PhaseCompleted); got != "parked" {
		t.Fatalf("terminal trajectory = %q, want parked", got)
	}
}

// TestProjectionMatchesTheRunContract checks the projected values against what
// the read contract says a run summary means.
func TestProjectionMatchesTheRunContract(t *testing.T) {
	p := ProjectRun(testIdentity(), Projection{}, completedRunEvents())
	run := p.Run

	if run.Phase != journal.PhaseCompleted {
		t.Errorf("phase = %q, want completed", run.Phase)
	}
	if !run.Terminal {
		t.Error("terminal = false for a completed run")
	}
	if run.CurrentStage != "" {
		t.Errorf("current stage = %q, want empty for a finished run", run.CurrentStage)
	}
	if run.FinishedAt == nil {
		t.Fatal("finished_at is nil for a completed run")
	}
	if run.LastSeq != 7 {
		t.Errorf("last_seq = %d, want 7", run.LastSeq)
	}
	// One policy retry, and it counts toward the total but not toward infra.
	if run.PolicyRetryCount != 1 || run.RetryCount != 1 || run.InfraRetryCount != 0 {
		t.Errorf("retries = policy %d / total %d / infra %d, want 1/1/0",
			run.PolicyRetryCount, run.RetryCount, run.InfraRetryCount)
	}
	// Gates are stages for filtering purposes: a stage filter must find "review".
	want := []string{"implement", "review"}
	if !reflect.DeepEqual(run.Stages, want) {
		t.Errorf("stages = %v, want %v", run.Stages, want)
	}
	if len(p.Stages) != 1 || p.Stages[0].Stage != "implement" || p.Stages[0].Attempts != 2 {
		t.Errorf("stage rows = %+v, want one 'implement' row with 2 attempts", p.Stages)
	}
	if !p.Stages[0].HadSuccess || !p.Stages[0].HadFailure {
		t.Errorf("attempt status set = %+v, want both success and failure", p.Stages[0])
	}
}

func TestStageOutcomeMatchesAnyAttempt(t *testing.T) {
	store := openTestStore(t)
	projection := ProjectRun(testIdentity(), Projection{}, completedRunEvents())
	if err := store.UpsertRun(t.Context(), projection); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}

	for _, outcome := range []Outcome{OutcomeSuccess, OutcomeFailure} {
		page, err := store.ListRuns(t.Context(), ListOptions{
			Stage: "implement", Outcome: outcome,
		})
		if err != nil {
			t.Fatalf("list outcome %q: %v", outcome, err)
		}
		if len(page.Runs) != 1 || page.Runs[0].RunID != testIdentity().RunID {
			t.Errorf("outcome %q returned runs %+v, want retried run", outcome, page.Runs)
		}
	}
}

func TestTerminalRunClosesOpenAttemptAsFailure(t *testing.T) {
	events := []journal.Event{
		ev(1, time.Second, journal.EventStageStarted, func(e *journal.Event) {
			e.Stage = "implement"
		}),
		ev(2, 2*time.Second, journal.EventRunFinished, func(e *journal.Event) {
			e.Status = string(journal.PhaseFailed)
		}),
	}
	projection := ProjectRun(testIdentity(), Projection{}, events)
	if len(projection.Stages) != 1 || !projection.Stages[0].HadFailure {
		t.Fatalf("stage rows = %+v, want open attempt closed as failure", projection.Stages)
	}
}

func TestExecutorErrorSynthesizesFailedStageAttempt(t *testing.T) {
	events := []journal.Event{
		ev(1, time.Second, journal.EventError, func(e *journal.Event) {
			e.Stage = "implement"
			e.Attempt = 1
			e.Error = &journal.ErrorDetail{Code: "executor_error", Message: "worker disappeared"}
		}),
		ev(2, 2*time.Second, journal.EventRunFinished, func(e *journal.Event) {
			e.Status = string(journal.PhaseFailed)
		}),
	}
	projection := ProjectRun(testIdentity(), Projection{}, events)
	if len(projection.Stages) != 1 {
		t.Fatalf("stage rows = %+v, want synthesized executor-error attempt", projection.Stages)
	}
	stage := projection.Stages[0]
	if stage.Stage != "implement" || stage.Attempts != 1 || stage.LastStatus != "failure" || !stage.HadFailure {
		t.Fatalf("stage row = %+v, want one failed implement attempt", stage)
	}

	store := openTestStore(t)
	if err := store.UpsertRun(t.Context(), projection); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}
	for _, outcome := range []Outcome{OutcomeFailure, OutcomeTerminal, OutcomeFinished} {
		page, err := store.ListRuns(t.Context(), ListOptions{Stage: "implement", Outcome: outcome})
		if err != nil {
			t.Fatalf("list outcome %q: %v", outcome, err)
		}
		if len(page.Runs) != 1 || page.Runs[0].RunID != testIdentity().RunID {
			t.Errorf("outcome %q returned runs %+v, want executor-error run", outcome, page.Runs)
		}
	}
}

// singleStageEvents builds a completed run touching exactly one stage, whose
// terminal status is the given one — the shape a routine backlog-query no-work
// tick and a genuine single-task success both produce, distinguished only by
// that status (#2188).
func singleStageEvents(status string) []journal.Event {
	return []journal.Event{
		ev(1, time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "query-backlog" }),
		ev(2, 2*time.Second, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Status = "query-backlog", status
		}),
		ev(3, 3*time.Second, journal.EventRunFinished, func(e *journal.Event) {
			e.Status = string(journal.PhaseCompleted)
		}),
	}
}

// TestProjectionClassifiesSingleStageNoWork is the regression test for #2188:
// the portal's run list needs to tell a routine no-work schedule tick apart
// from a genuine single-stage success, using only the signal the runner
// already records (a stage's own terminal status).
func TestProjectionClassifiesSingleStageNoWork(t *testing.T) {
	noWork := ProjectRun(testIdentity(), Projection{}, singleStageEvents("no-work"))
	if noWork.Run.Disposition != DispositionNoWork {
		t.Errorf("disposition = %q, want %q for a single no-work stage", noWork.Run.Disposition, DispositionNoWork)
	}

	success := ProjectRun(testIdentity(), Projection{}, singleStageEvents("success"))
	if success.Run.Disposition != DispositionUnknown {
		t.Errorf("disposition = %q, want %q for a single successful stage — a real single-task workflow is not no-work",
			success.Run.Disposition, DispositionUnknown)
	}

	// completedRunEvents touches two stages (implement, review); even though
	// "implement" itself succeeds, a multi-stage run must never be classified
	// no-work regardless of any individual stage's status.
	multiStage := ProjectRun(testIdentity(), Projection{}, completedRunEvents())
	if multiStage.Run.Disposition != DispositionUnknown {
		t.Errorf("disposition = %q, want %q for a multi-stage run", multiStage.Run.Disposition, DispositionUnknown)
	}
}

// TestResumeReopensATerminalRun pins the case that would otherwise leave a live
// run looking finished to every list.
//
// runner.ResumeFromTerminal durably reopens an escalated or failed run. If
// finished_at survived the resume, the run would list as terminal while actually
// executing — and it would also be excluded from the active-run count, which is
// the number the scheduler's concurrency ceiling is compared against.
func TestResumeReopensATerminalRun(t *testing.T) {
	identity := testIdentity()
	events := append(completedRunEvents(),
		ev(8, 8*time.Second, journal.EventRunResumed, func(e *journal.Event) { e.Target = "implement" }),
	)
	run := ProjectRun(identity, Projection{}, events).Run

	if run.Phase != journal.PhaseRunning {
		t.Errorf("phase = %q after resume, want running", run.Phase)
	}
	if run.Terminal {
		t.Error("terminal = true after resume")
	}
	if run.FinishedAt != nil {
		t.Errorf("finished_at = %v after resume, want nil; a resumed run that keeps it lists as finished while executing", run.FinishedAt)
	}
	if run.CurrentStage != "implement" {
		t.Errorf("current stage = %q after resume, want implement", run.CurrentStage)
	}
}

func TestGateOverrideReopensATerminalRun(t *testing.T) {
	events := append(completedRunEvents(),
		ev(8, 8*time.Second, journal.EventGateOverridden, func(e *journal.Event) {
			e.Gate, e.Verdict, e.Target = "review", "needs-changes", "implement"
		}),
	)
	run := ProjectRun(testIdentity(), Projection{}, events).Run
	if run.Phase != journal.PhaseRunning || run.Terminal || run.FinishedAt != nil || run.CurrentStage != "implement" {
		t.Fatalf("run after gate override = %+v, want live at implement", run)
	}
}

func TestTerminalGateOverridePreservesOutcome(t *testing.T) {
	events := append(completedRunEvents(),
		ev(8, 8*time.Second, journal.EventGateOverridden, func(e *journal.Event) {
			e.Gate, e.Verdict, e.Target = "review", "pass", journal.TargetComplete
		}),
		ev(9, 9*time.Second, journal.EventRunFinished, func(e *journal.Event) {
			e.Status = string(journal.PhaseCompleted)
		}),
	)
	run := ProjectRun(testIdentity(), Projection{}, events).Run

	if run.OutcomeVerdict != "pass" || run.OutcomeTarget != journal.TargetComplete {
		t.Fatalf("outcome = %q/%q, want terminal override decision", run.OutcomeVerdict, run.OutcomeTarget)
	}
}

// TestUnrecognisedTerminalStatusDoesNotCorruptPhase pins the refusal.
//
// phase is an indexed, filtered column. Writing an unrecognised terminal status
// into it would make the run vanish from every phase-filtered list — a silent
// omission (§14.7) rather than a visible error.
func TestUnrecognisedTerminalStatusDoesNotCorruptPhase(t *testing.T) {
	identity := testIdentity()
	events := []journal.Event{
		ev(1, time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		ev(2, 2*time.Second, journal.EventRunFinished, func(e *journal.Event) { e.Status = "not-a-phase" }),
	}
	run := ProjectRun(identity, Projection{}, events).Run

	if run.Phase != journal.PhaseRunning {
		t.Errorf("phase = %q, want running left intact rather than an unrecognised value", run.Phase)
	}
	if run.Terminal {
		t.Error("run marked terminal from an unrecognised status")
	}
	// The sequence still advanced: the event happened even if its meaning was
	// not accepted, and pretending otherwise would make the run look stale.
	if run.LastSeq != 2 {
		t.Errorf("last_seq = %d, want 2", run.LastSeq)
	}
}

// TestUpsertIsIdempotentAndNeverRewinds pins the write guard.
//
// Re-applying an older projection must not overwrite a newer one. Without the
// last_seq guard a repair sweep racing live projection could rewind a run's
// phase — turning a finished run back into a running one, which is #1943's
// symptom arriving by a different route.
func TestUpsertIsIdempotentAndNeverRewinds(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	identity := testIdentity()
	events := completedRunEvents()
	full := ProjectRun(identity, Projection{}, events)
	if err := store.UpsertRun(ctx, full); err != nil {
		t.Fatalf("upsert full: %v", err)
	}

	// Re-apply identically: a no-op.
	if err := store.UpsertRun(ctx, full); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, ok, err := store.GetRun(ctx, identity.RunID)
	if err != nil || !ok {
		t.Fatalf("get run: ok=%v err=%v", ok, err)
	}
	if got.Phase != journal.PhaseCompleted || got.LastSeq != 7 {
		t.Fatalf("after re-upsert: phase=%q last_seq=%d, want completed/7", got.Phase, got.LastSeq)
	}

	// Now an OLDER projection — the run as it was mid-flight.
	stale := ProjectRun(identity, Projection{}, events[:2])
	if err := store.UpsertRun(ctx, stale); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}
	got, ok, err = store.GetRun(ctx, identity.RunID)
	if err != nil || !ok {
		t.Fatalf("get run after stale: ok=%v err=%v", ok, err)
	}
	if got.Phase != journal.PhaseCompleted {
		t.Errorf("phase = %q after applying an older projection; the last_seq guard did not hold, "+
			"and a repair sweep racing live projection could rewind a finished run", got.Phase)
	}
	if got.LastSeq != 7 {
		t.Errorf("last_seq = %d after applying an older projection, want 7", got.LastSeq)
	}
}

// TestCountByPhaseIsAnIndexedAggregate pins §5.4's replacement for the directory
// walk: "stored, that becomes one indexed aggregate over phase = 'running'".
func TestCountByPhaseIsAnIndexedAggregate(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Two finished runs and one still in flight.
	for i, events := range [][]journal.Event{
		completedRunEvents(),
		completedRunEvents(),
		completedRunEvents()[:3], // no run.finished
	} {
		identity := testIdentity()
		identity.RunID = string(rune('a'+i)) + "-run"
		if err := store.UpsertRun(ctx, ProjectRun(identity, Projection{}, events)); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	counts, err := store.CountByPhase(ctx)
	if err != nil {
		t.Fatalf("count by phase: %v", err)
	}
	if counts[journal.PhaseRunning] != 1 {
		t.Errorf("running = %d, want 1", counts[journal.PhaseRunning])
	}
	if counts[journal.PhaseCompleted] != 2 {
		t.Errorf("completed = %d, want 2", counts[journal.PhaseCompleted])
	}
}

func TestActiveRunCountsGroupsProjectedRunningRunsByWorkflow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for i, events := range [][]journal.Event{
		completedRunEvents()[:3],
		completedRunEvents()[:3],
		completedRunEvents(),
	} {
		identity := testIdentity()
		identity.RunID = fmt.Sprintf("run-%d", i)
		if i == 1 {
			identity.Workflow = "other-workflow"
		}
		if err := store.UpsertRun(ctx, ProjectRun(identity, Projection{}, events)); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	counts, err := store.ActiveRunCounts(ctx)
	if err != nil {
		t.Fatalf("active run counts: %v", err)
	}
	if len(counts) != 2 {
		t.Fatalf("counts = %#v, want two active workflows", counts)
	}
	got := map[string]int{}
	for _, count := range counts {
		got[count.Gaggle+"/"+count.Workflow] = count.Count
	}
	if got["alpha/implementation"] != 1 || got["alpha/other-workflow"] != 1 {
		t.Errorf("counts = %#v, want one active run for each workflow", got)
	}
}

// TestBuildFromJournalsProjectsEveryPublishedRun pins §6.6 step 2 — the build
// path — and the two properties that make it safe to run on a real instance.
func TestBuildFromJournalsProjectsEveryPublishedRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Three published runs and one unpublished directory. The unpublished one is
	// not an edge case: 10,906 of the live instance's 40,665 directories (27%)
	// have no run.yaml and can never be ingested.
	for i := 0; i < 3; i++ {
		writeRunJournal(t, runsDir, fmt.Sprintf("%032x", i), i == 2)
	}
	if err := os.MkdirAll(filepath.Join(runsDir, "unpublished-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := Open(filepath.Join(root, FileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	result, err := store.BuildFromJournals(ctx, []string{runsDir})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.Projected != 3 {
		t.Errorf("projected %d runs, want 3", result.Projected)
	}
	if result.Skipped != 1 {
		t.Errorf("skipped %d directories, want 1 (the unpublished one)", result.Skipped)
	}

	// The phase aggregate — §5.4's replacement for the 17.2 s directory walk —
	// must be answerable from the store alone.
	counts, err := store.CountByPhase(ctx)
	if err != nil {
		t.Fatalf("count by phase: %v", err)
	}
	if counts[journal.PhaseCompleted] != 2 || counts[journal.PhaseRunning] != 1 {
		t.Errorf("phase counts = %v, want 2 completed and 1 running", counts)
	}

	// Rebuilding over an existing store is idempotent, which is what makes an
	// interrupted build resumable without bookkeeping.
	second, err := store.BuildFromJournals(ctx, []string{runsDir})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if second.Projected != result.Projected {
		t.Errorf("second build projected %d, want %d", second.Projected, result.Projected)
	}
	counts, err = store.CountByPhase(ctx)
	if err != nil {
		t.Fatalf("count after rebuild: %v", err)
	}
	if counts[journal.PhaseCompleted] != 2 || counts[journal.PhaseRunning] != 1 {
		t.Errorf("phase counts after rebuild = %v, want unchanged", counts)
	}
}

// writeRunJournal creates a real run journal through the production API, so the
// build path is exercised against the on-disk format rather than a fixture that
// could drift from it.
func writeRunJournal(t *testing.T, runsDir, runID string, leaveRunning bool) {
	t.Helper()
	clock := projectBase
	tick := func() time.Time { clock = clock.Add(time.Second); return clock }
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 3,
		Gaggle:          "alpha",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       clock,
	}, nil, journal.WithClock(tick))
	if err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "success",
	}); err != nil {
		t.Fatal(err)
	}
	if !leaveRunning {
		if err := run.Append(journal.Event{
			Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestProjectRunDirKeepsTheStoreCurrent pins the writer seam.
//
// Without it the read model would only be as fresh as its last rebuild, which is
// not a basis for serving reads: a run that finished thirty seconds ago would be
// missing or still marked running, and the portal's first question is "what
// needs me?".
//
// This is the interim mechanism. §6.1 requires the projector to be driven by a
// durable intake watermark rather than an in-process call from the writer,
// precisely so runs written while the daemon was down are not invisible — the
// same limitation today's IngestRun-on-finish coupling has, and why repair
// exists. Wave 3 replaces it.
func TestProjectRunDirKeepsTheStoreCurrent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("%032x", 7)
	writeRunJournal(t, runsDir, runID, true) // still running

	store, err := Open(filepath.Join(root, FileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	dir := filepath.Join(runsDir, runID)
	if err := store.ProjectRunDir(ctx, dir); err != nil {
		t.Fatalf("project running run: %v", err)
	}
	counts, err := store.CountByPhase(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts[journal.PhaseRunning] != 1 {
		t.Fatalf("running count = %d after projecting an in-flight run, want 1", counts[journal.PhaseRunning])
	}

	// The run finishes; the same seam must move it out of the running set.
	run, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatalf("reopen run: %v", err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	if err := store.ProjectRunDir(ctx, dir); err != nil {
		t.Fatalf("project finished run: %v", err)
	}
	counts, err = store.CountByPhase(ctx)
	if err != nil {
		t.Fatalf("counts after finish: %v", err)
	}
	if counts[journal.PhaseRunning] != 0 || counts[journal.PhaseCompleted] != 1 {
		t.Errorf("after finishing: running=%d completed=%d, want 0 and 1.\n"+
			"A run that stays 'running' after finishing inflates the active-run count the "+
			"concurrency ceiling is compared against.",
			counts[journal.PhaseRunning], counts[journal.PhaseCompleted])
	}
}
