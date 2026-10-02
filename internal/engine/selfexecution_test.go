package engine

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func TestSelfExecutionDeniedActivitiesRefuseBeforeSideEffects(t *testing.T) {
	acts := &Activities{AdmitSelfExecution: func(stage string) error { return &runner.SelfExecutionRefusal{Stage: stage} }}
	env := apiv1.InvocationEnvelope{TaskID: "run:work"}
	agent, err := acts.InvokeGoober(context.Background(), env, "", "", apiv1.WorkspaceRepo, "")
	if err != nil || agent.Status != apiv1.ResultBlocked || agent.Error.Code != runner.SelfExecutionDeniedCode || agent.SelfPlacement != nil {
		t.Fatalf("agent: %+v %v", agent, err)
	}
	det, err := acts.RunDeterministic(context.Background(), env, apiv1.DeterministicRun{}, "", "")
	if err != nil || det.Status != apiv1.ResultBlocked || det.Error.Code != runner.SelfExecutionDeniedCode || det.SelfPlacement != nil {
		t.Fatalf("deterministic: %+v %v", det, err)
	}
	_, err = acts.ReviewGoober(context.Background(), env, "", "", apiv1.WorkspaceRepo, "", true)
	if !IsSelfExecutionDenied(err) {
		t.Fatalf("review: %v", err)
	}
}

// Every work stage and the reviewer crosses the real dispatch activity and
// surrender boundary. The local activity guard is deliberately fatal: a lost
// pin cannot masquerade as a successful zero-self lifecycle.
func TestSelfExecutionDeniedFullDispatchedLifecycle(t *testing.T) {
	in := placedGateInput("self-denied-lifecycle")
	in.SelfExecutionDenied = true
	in.Placements = []PinnedPlacement{remotePin("implement"), remoteGatePin(), remotePin("land")}
	store := surrenderStore(t)
	for _, stage := range []string{"implement", "land"} {
		putSurrendered(t, store, in.RunID, stage, 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "completed dispatched work"}})
	}
	putSurrendered(t, store, in.RunID, "review", 1, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "approved", Rationale: "reviewed implementation"}))
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-cli", Pod: "work-pod", Image: "example/worker:v1", Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true}}
	proj := executeForProjection(t, in, &Activities{Dispatcher: fake, Surrenders: store, AdmitSelfExecution: func(stage string) error {
		t.Errorf("self selected for %s", stage)
		return &runner.SelfExecutionRefusal{Stage: stage}
	}}, false)
	attempts, _ := fake.recorded()
	if len(attempts) != 3 {
		t.Fatalf("attempts: %+v", attempts)
	}
	placements := projectedPlacements(t, proj)
	if len(placements) != 3 {
		t.Fatalf("placement observations: %+v", placements)
	}
	for _, placement := range placements {
		if placement.Runner == journal.PlacementRunnerSelf {
			t.Fatalf("daemon work: %+v", placement)
		}
	}
}

func TestSelfExecutionDeniedPinnedPolicyRejectsAbsentAndSelfPins(t *testing.T) {
	for _, pins := range [][]PinnedPlacement{nil, {{Stage: "build", Self: true}}} {
		in := projectionInput("denied-pin", apiv1.WorkflowSpec{Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "build", Tasks: []apiv1.Task{podTask("build", "", nil)}})
		in.SelfExecutionDenied = true
		in.Placements = pins
		proj := executeForProjection(t, in, &Activities{AdmitSelfExecution: func(stage string) error { t.Fatalf("lost pin reached a local activity: %s", stage); return nil }}, false)
		if placements := projectedPlacements(t, proj); len(placements) != 0 {
			t.Fatalf("refusal ran locally: %+v", placements)
		}
	}
}

func TestSelfExecutionDeniedDispatchBoundary(t *testing.T) {
	acts := &Activities{Dispatcher: &fakeStageDispatcher{}, Surrenders: surrenderStore(t), AdmitSelfExecution: func(stage string) error { return &runner.SelfExecutionRefusal{Stage: stage} }}
	err := acts.validateStageDispatch(DispatchStageInput{Envelope: apiv1.InvocationEnvelope{TaskID: "run:work"}, Placement: PinnedPlacement{Self: true}})
	if !IsSelfExecutionDenied(err) {
		t.Fatalf("dispatch self selection lost policy classification: %v", err)
	}
}

func TestSelfExecutionUnavailableIsNotPolicyRefusal(t *testing.T) {
	acts := &Activities{AdmitSelfExecution: func(string) error { return ErrNotConfigured }}
	_, err := acts.InvokeGoober(context.Background(), apiv1.InvocationEnvelope{}, "", "", apiv1.WorkspaceRepo, "")
	if err == nil || IsSelfExecutionDenied(err) {
		t.Fatalf("configuration error mislabeled as policy: %v", err)
	}
	_, err = acts.ReviewGoober(context.Background(), apiv1.InvocationEnvelope{}, "", "", apiv1.WorkspaceRepo, "", false)
	if err == nil || IsSelfExecutionDenied(err) {
		t.Fatalf("review configuration error mislabeled as policy: %v", err)
	}
}
