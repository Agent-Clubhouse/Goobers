package engine

import (
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
)

// dispatchowner_test.go pins the identity the worker's orphan sweep deletes
// on: dispatcher.Attempt.OwningWorkflowID, the id of the workflow execution
// whose activity created the stage pod.
//
// The sweep used to COMPOSE that address from the pod's run/stage/attempt.
// These tests exist because that is unsound for a scheduled run, and unsound
// in the one direction that destroys work: every composed id names an
// execution that does not exist, Temporal answers NotFound, and a resolver
// reading NotFound as "settled" deletes the pod of a stage that is still
// running. So the driver states its own id here, at dispatch, and the sweep
// describes it verbatim.

// remotelyPlacedSpec is a one-stage workflow whose only stage is pinned to a
// non-self runner, i.e. routed through ActDispatchStage.
func remotelyPlacedSpec(stage string) apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "0 3 * * *"}},
		Start:    stage,
		Tasks: []apiv1.Task{{
			Name: stage, Type: apiv1.TaskDeterministic, Goal: stage,
			Run: &apiv1.DeterministicRun{Command: []string{"gh", "pr", "create"}, Workspace: apiv1.WorkspaceScratch},
		}},
	}
}

func remotePlacement(stage string) []PinnedPlacement {
	return []PinnedPlacement{{
		Stage: stage, Queue: dispatcher.QueueName("web", "win-ci"),
		Eligible: remoteEligible(), Memory: "4Gi",
	}}
}

func succeedingStageDispatcher() *fakeStageDispatcher {
	return &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "win-ci", Pod: "pod-1", Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true,
	}}
}

// A SCHEDULED run's stage pod must carry its Run workflow's real id —
// claimID+"-run" — and not the run id, which RunScheduled has rewritten to a
// hash of the claim id.
//
// This is the whole regression, driven end to end: RunScheduled -> the walk ->
// ActDispatchStage -> the dispatcher.Attempt a pod is rendered from. Every
// hop that dropped the driver would show up here.
func TestScheduledRunStampsItsOwnWorkflowIDOnTheAttempt(t *testing.T) {
	const stage = "open-pr"
	scheduleID := ScheduleID("prod-west", "web", "nightly", 0)
	claimID := ScheduleClaimID(scheduleID, time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC))
	// The production shape: ClaimScheduled starts the child as
	// claimID+"-run" and hands it the claim id as RunInput.RunID.
	workflowID := scheduledRunWorkflowID(claimID)

	in := runInput("nightly", remotelyPlacedSpec(stage))
	in.RunID = claimID
	in.TriggerRef = scheduleID
	in.Placements = remotePlacement(stage)

	// RunScheduled rewrites RunID to this hash before the walk dispatches
	// anything, so the surrendered result is keyed by it too.
	hashedRunID := RunID(claimID)
	store := surrenderStore(t)
	putSurrendered(t, store, hashedRunID, stage, 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "opened"},
	})
	fake := succeedingStageDispatcher()

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: workflowID})
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: store})
	env.ExecuteWorkflow(RunScheduled, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}

	attempts, _ := fake.recorded()
	if len(attempts) != 1 {
		t.Fatalf("dispatched %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.OwningWorkflowID != workflowID {
		t.Fatalf("attempt.OwningWorkflowID = %q, want the run workflow's own id %q — the pod would name no describable driver",
			got.OwningWorkflowID, workflowID)
	}
	// The pin that makes the assertion above mean something: the run id the
	// pod also carries is a hash, so no rule turns it into the driver.
	if got.RunID != hashedRunID {
		t.Fatalf("attempt.RunID = %q, want the rewritten hash %q", got.RunID, hashedRunID)
	}
	if got.RunID == got.OwningWorkflowID || strings.Contains(got.OwningWorkflowID, got.RunID) {
		t.Fatalf("run id %q reconstructs the driver %q — the scheduled shape is precisely that it cannot, so this test would pass on a composing implementation",
			got.RunID, got.OwningWorkflowID)
	}
}

// The ordinary engine-start shape, for contrast: the Run workflow's id IS the
// run id, so composing happened to work here. Asserted so the fix is known to
// keep answering correctly for the case that already did.
func TestEngineRunStampsItsOwnWorkflowIDOnTheAttempt(t *testing.T) {
	const stage = "build"
	in := runInput("mode-three-owner", remotelyPlacedSpec(stage))
	in.Placements = remotePlacement(stage)

	store := surrenderStore(t)
	putSurrendered(t, store, in.RunID, stage, 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "built"},
	})
	fake := succeedingStageDispatcher()

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: in.RunID})
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: store})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	attempts, _ := fake.recorded()
	if len(attempts) != 1 {
		t.Fatalf("dispatched %d attempts, want 1", len(attempts))
	}
	if got := attempts[0].OwningWorkflowID; got != in.RunID {
		t.Fatalf("attempt.OwningWorkflowID = %q, want the run workflow's id %q", got, in.RunID)
	}
}

// DispatchOne stamps ITS OWN execution id, overwriting whatever the caller
// put in the payload.
//
// The daemon builds the DispatchStageInput and starts this workflow, so the
// field arrives caller-controlled. A caller that named a workflow it knew had
// COMPLETED would hand the sweep a licence to delete this pod while its stage
// was still running; refuseUnboundAttemptIdentity takes the same posture
// about the payload's attempt identity a few lines above.
func TestDispatchOneOverwritesCallerSuppliedOwningWorkflowID(t *testing.T) {
	in := dispatchInput("run-dispatch-owner", "build", 1)
	in.Run = &apiv1.DeterministicRun{Command: []string{"build.cmd"}, Workspace: apiv1.WorkspaceScratch}
	in.Placement.LedgerTouching = false
	in.OwningWorkflowID = "some-long-completed-workflow"
	workflowID := DispatchOneWorkflowID("run-dispatch-owner", "build", 1)

	store := surrenderStore(t)
	putSurrendered(t, store, "run-dispatch-owner", "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "built in a pod"},
	})
	fake := succeedingStageDispatcher()

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: workflowID})
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: store})
	env.ExecuteWorkflow(DispatchOne, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("DispatchOne: %v", err)
	}

	attempts, _ := fake.recorded()
	if len(attempts) != 1 {
		t.Fatalf("dispatched %d attempts, want 1", len(attempts))
	}
	if got := attempts[0].OwningWorkflowID; got != workflowID {
		t.Fatalf("attempt.OwningWorkflowID = %q, want this execution's own id %q — a caller-supplied driver is not evidence",
			got, workflowID)
	}
}

// An activity invoked with no stated driver stamps none rather than
// inventing one. The dispatcher then renders a pod without the annotation,
// which its sweep refuses to address at all — the fail-toward-leaving
// direction, end to end.
func TestDispatchStageCarriesNoDriverWhenTheInputStatesNone(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "run-undriven", "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := succeedingStageDispatcher()
	a := &Activities{Dispatcher: fake, Surrenders: store}
	input := dispatchInput("run-undriven", "build", 1)
	input.Run = &apiv1.DeterministicRun{Command: []string{"build.cmd"}, Workspace: apiv1.WorkspaceScratch}
	input.Placement.LedgerTouching = false

	if _, err := a.DispatchStage(t.Context(), input); err != nil {
		t.Fatalf("DispatchStage: %v", err)
	}
	attempts, _ := fake.recorded()
	if len(attempts) != 1 {
		t.Fatalf("dispatched %d attempts, want 1", len(attempts))
	}
	if got := attempts[0].OwningWorkflowID; got != "" {
		t.Fatalf("attempt.OwningWorkflowID = %q for an input naming no driver, want empty — a fabricated address on a delete path is the failure this field removed", got)
	}
}
