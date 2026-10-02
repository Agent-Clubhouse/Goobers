package engine

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	temporaltest "github.com/goobers/goobers/internal/temporaltest"
)

func TestEnginePinsNamedArtifactPublicationForBothExecutors(t *testing.T) {
	spec := apiv1.WorkflowSpec{Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "det", Tasks: []apiv1.Task{
		{Name: "det", Type: apiv1.TaskDeterministic, Goal: "publish", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, ArtifactSlots: []apiv1.ArtifactSlot{{Name: "report"}}, Next: "agent"},
		{Name: "agent", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "publish", ArtifactSlots: []apiv1.ArtifactSlot{{Name: "evidence"}}},
	}}
	in := runInput("named-slots", spec)
	in.DSLVersion = "3.1"
	det := &capturingDeterministic{}
	var captured apiv1.InvocationEnvelope
	inv := &fakeInvoker{invoke: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		captured = env
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Goober: inv, Det: det, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	for _, invocation := range []apiv1.InvocationEnvelope{det.captured()[0], captured} {
		if invocation.ArtifactPublication == nil || invocation.ArtifactPublication.Visit == 0 || len(invocation.ArtifactPublication.Slots) != 1 || invocation.Attempt != 1 {
			t.Fatalf("envelope=%+v", invocation)
		}
	}
}

func TestEngineNamedSlotVisitChangesOnRetryAndReentry(t *testing.T) {
	task := apiv1.Task{Name: "produce", ArtifactSlots: []apiv1.ArtifactSlot{{Name: "report"}}}
	rec := &runJournal{}
	previous := uint64(0)
	for _, attempt := range []int{1, 2, 1} {
		rec.stageStarted(time.Now(), task, attempt, journal.AttemptPolicy)
		env := rec.taskAttemptEnvelope(apiv1.InvocationEnvelope{}, task, attempt)
		if env.ArtifactPublication.Visit <= previous || env.Attempt != int32(attempt) {
			t.Fatalf("visit reused: %+v", env)
		}
		if got := rec.proj.Ops[len(rec.proj.Ops)-1].Event.Runner["artifactVisit"]; got != env.ArtifactPublication.Visit {
			t.Fatalf("journal=%v envelope=%+v", got, env)
		}
		previous = env.ArtifactPublication.Visit
	}
}

func TestEngineNamedPublicationPreservesOldHistory(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.OnGetVersion(namedPublicationChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	env.ExecuteWorkflow(func(ctx workflow.Context) (bool, error) {
		task := namedPublicationTask(ctx, apiv1.Task{Name: "produce", ArtifactSlots: []apiv1.ArtifactSlot{{Name: "report"}}})
		rec := &runJournal{}
		rec.stageStarted(workflow.Now(ctx), task, 1, "")
		return rec.artifactPublication(task) == nil && rec.proj.Ops[0].Event.Runner == nil, nil
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err := env.GetWorkflowResult(&preserved); err != nil {
		t.Fatal(err)
	}
	if !preserved {
		t.Fatal("old history gained named publication")
	}
}
