package engine

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// headmoved6019_test.go is the engine half of #6019: a pod that refuses to
// land the run's workspace delta because the pull request head the run
// rebound to moved under it (someone pushed while the run worked, #5948)
// surrenders runner.WorkspaceHeadMovedErrorCode, and the walk ends the run
// completed — superseded — instead of failed. The journaled marker is what
// the terminal handlers read to leave the moved PR's streaks and budget alone.

func headMovedSurrender() dispatcher.SurrenderedResult {
	return dispatcher.SurrenderedResult{WorkspaceHeadMoved: true, Result: apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error: &apiv1.ErrorInfo{
			Code:    runsemantics.WorkspaceHeadMovedErrorCode,
			Message: "workspace delta sha256:d: ref is at aaaa, delta carries bbbb (the ref carries 1 commit(s) the delta lacks: aaaa person's push)",
		},
	}}
}

// headMovedMarkers returns the workspace_head_moved error events the run
// journaled, by stage.
func headMovedMarkers(proj JournalProjection) []string {
	var stages []string
	for _, op := range proj.Ops {
		if op.Kind == opAppend && op.Event != nil && op.Event.Type == journal.EventError &&
			op.Event.Error != nil && op.Event.Error.Code == runsemantics.WorkspaceHeadMovedErrorCode {
			stages = append(stages, op.Event.Stage)
		}
	}
	return stages
}

func TestDispatchStageReviewHeadMovedIsTypedAndNeverRetried(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "run-moved", "review", 1, headMovedSurrender())
	a := &Activities{Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}, Surrenders: store}
	input := dispatchInput("run-moved", "review", 1)
	input.Review = true

	_, err := a.DispatchStage(context.Background(), input)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != FailureTypeWorkspaceHeadMoved {
		t.Fatalf("error = %v, want a %s application error", err, FailureTypeWorkspaceHeadMoved)
	}
	if !appErr.NonRetryable() {
		t.Fatal("head-moved review failure is retryable; a fresh pod clones the same moved head")
	}
	if !IsWorkspaceHeadMoved(err) {
		t.Fatal("IsWorkspaceHeadMoved = false for the typed review failure")
	}
	if IsWorkspaceHeadMoved(temporal.NewApplicationError("x", FailureTypeStage)) {
		t.Fatal("IsWorkspaceHeadMoved = true for an ordinary stage failure")
	}
}

// The walk's gate arm: the reviewer pod could not land the run's delta on the
// moved head, so there is no verdict and nothing to review. The run ends
// completed at the gate — not failed, not retried on the gate's evaluator
// budget, and not routed down any branch.
func TestPlacedGateHeadMovedEndsTheRunSuperseded(t *testing.T) {
	in := placedGateInput("placed-gate-head-moved")
	in.Placements = []PinnedPlacement{remoteGatePin()}
	in.Spec.Gates[0].Agentic.Retry = &apiv1.RetryPolicy{MaxAttempts: 3}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "review", 1, headMovedSurrender())
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-agentic", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	landed := false
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		landed = true
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}

	result, proj := executeForResult(t, in, &Activities{
		Goober: refusingReviewer(t), Det: det, Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders,
	})
	if result.Status != StatusCompleted || result.FinalState != "review" || result.FailureCode != "" || result.NoWork {
		t.Fatalf("result = %+v, want a completion at review with no failure code, not a first-stage no-work tick (scheduler idle backoff)", result)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("dispatcher calls = %d, want one: the refusal is never retried on the evaluator budget", got)
	}
	if landed {
		t.Fatal("the run routed past the gate on a review that never happened")
	}
	if got := gateEvaluatedEvents(proj); len(got) != 0 {
		t.Fatalf("gate.evaluated = %+v, want none: no verdict was reached", got)
	}
	if got := headMovedMarkers(proj); len(got) != 1 || got[0] != "review" {
		t.Fatalf("head-moved markers = %v, want one at review", got)
	}
}

// The walk's task arm: a task whose surrendered failure is the head-moved code
// ends the run superseded before its failure-class gate (or continueOnError)
// can route it as an ordinary failure.
func TestTaskHeadMovedEndsTheRunSupersededBeforeGateRouting(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "local-ci",
		Tasks: []apiv1.Task{
			{Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "ci", ContinueOnError: true, Next: "local-gate",
				Run: &apiv1.DeterministicRun{Command: []string{"make", "ci"}, Workspace: apiv1.WorkspaceRepo}},
			{Name: "push", Type: apiv1.TaskDeterministic, Goal: "push",
				Run: &apiv1.DeterministicRun{Command: []string{"push.sh"}, Workspace: apiv1.WorkspaceScratch}},
		},
		Gates: []apiv1.Gate{{
			Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "failure-class"},
			Branches:  map[string]string{"pass": "push", "fail": wf.TargetAbort, "infra": wf.TargetAbort},
		}},
	}
	pushed := false
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		pushed = true
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}
	in := projectionInput("task-head-moved", spec)
	in.Placements = []PinnedPlacement{remotePin("local-ci")}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "local-ci", 1, headMovedSurrender())
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	result, proj := executeForResult(t, in, &Activities{Det: det, Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	if result.Status != StatusCompleted || result.FinalState != "local-ci" || result.FailureCode != "" || result.NoWork {
		t.Fatalf("result = %+v, want a completion at local-ci with no failure code, not a first-stage no-work tick (scheduler idle backoff)", result)
	}
	if pushed {
		t.Fatal("the run continued past a task whose subject moved")
	}
	if got := gateEvaluatedEvents(proj); len(got) != 0 {
		t.Fatalf("gate.evaluated = %+v, want the gate never evaluated", got)
	}
	if got := headMovedMarkers(proj); len(got) != 1 || got[0] != "local-ci" {
		t.Fatalf("head-moved markers = %v, want one at local-ci", got)
	}
}

// The code is reserved for the pod runtime (review finding on #7403): a
// stage's own result claiming workspace_head_moved, without the runtime's
// attestation, fails like any other stage failure instead of skipping gate
// routing and every streak. Checked on both arms.
func TestUnattestedHeadMovedCodeIsAnOrdinaryFailure(t *testing.T) {
	t.Run("review", func(t *testing.T) {
		store := surrenderStore(t)
		forged := headMovedSurrender()
		forged.WorkspaceHeadMoved = false
		putSurrendered(t, store, "run-forged", "review", 1, forged)
		a := &Activities{Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}, Surrenders: store}
		input := dispatchInput("run-forged", "review", 1)
		input.Review = true
		_, err := a.DispatchStage(context.Background(), input)
		var appErr *temporal.ApplicationError
		if IsWorkspaceHeadMoved(err) || !errors.As(err, &appErr) || appErr.Type() != FailureTypeStage {
			t.Fatalf("error = %v, want an ordinary %s failure", err, FailureTypeStage)
		}
	})
	t.Run("task", func(t *testing.T) {
		store := surrenderStore(t)
		forged := headMovedSurrender()
		forged.WorkspaceHeadMoved = false
		putSurrendered(t, store, "run-forged-task", "implement", 1, forged)
		a := &Activities{Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}, Surrenders: store}
		result, err := a.DispatchStage(context.Background(), dispatchInput("run-forged-task", "implement", 1))
		if err != nil {
			t.Fatalf("DispatchStage: %v", err)
		}
		if result.Error == nil || result.Error.Code != reservedHeadMovedCode || workspaceHeadMovedResult(result.ResultEnvelope) {
			t.Fatalf("result error = %+v, want the reserved code renamed", result.Error)
		}
	})
	t.Run("in-process executor", func(t *testing.T) {
		result := apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: runsemantics.WorkspaceHeadMovedErrorCode, Message: "claimed"}}
		admitWorkspaceHeadMoved(&result, false)
		if workspaceHeadMovedResult(result) {
			t.Fatalf("result = %+v, want an unattested claim renamed", result.Error)
		}
	})
}

func executeForResult(t *testing.T, in RunInput, acts *Activities) (RunResult, JournalProjection) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want the run to end superseded, not failed", err)
	}
	var result RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("decode run result: %v", err)
	}
	val, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatalf("query projection: %v", err)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	return result, proj
}
