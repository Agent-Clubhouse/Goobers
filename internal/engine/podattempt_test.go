package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestNegativePhysicalAttemptRejectedBeforeDispatch(t *testing.T) {
	in := dispatchInput("physical-one", "build", 1)
	in.PodAttempt = -1
	_, activityErr := (&Activities{}).DispatchStage(context.Background(), in)
	workflowErr := refuseUnboundAttemptIdentity(DispatchOneWorkflowID(in.Envelope.RunID, "build", 1), in.Envelope, in.PodAttempt)
	for _, err := range []error{activityErr, workflowErr} {
		var appErr *temporal.ApplicationError
		if !errors.As(err, &appErr) || appErr.Type() != FailureTypeStage || !appErr.NonRetryable() {
			t.Fatalf("malformed physical attempt accepted or retryable: %v", err)
		}
	}
}

func TestPhysicalAttemptActivationUsesOneVersionMarker(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.OnGetVersion(mock.Anything, workflow.DefaultVersion, 1).Return(workflow.Version(1)).Once()
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = workflow.WithValue(ctx, podDispatchActivationKey{}, &podDispatchActivation{})
		for ordinal := 1; ordinal <= 100; ordinal++ {
			if got := dispatchPodAttempt(ctx, "task", ordinal); got != ordinal {
				t.Errorf("ordinal=%d", got)
			}
		}
		return nil
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	env.AssertExpectations(t)
}

func TestDispatchOneBindsPhysicalIdentityIndependentlyOfJournal(t *testing.T) {
	for _, physical := range []int{0, 7} {
		in := dispatchInput("physical-one", "build", 1)
		in.PodAttempt = physical
		in.Placement.LedgerTouching = false
		in.Run = &apiv1.DeterministicRun{Command: []string{"build"}, Workspace: apiv1.WorkspaceScratch}
		d := succeedingStageDispatcher()
		plane := surrenderStore(t)
		identity := (dispatcher.Attempt{Number: 1, PodAttempt: physical}).IdentityAttempt()
		putSurrendered(t, plane, in.Envelope.RunID, "build", identity, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
		var suite testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&suite)
		env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: DispatchOneWorkflowID(in.Envelope.RunID, "build", identity)})
		env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: d, Surrenders: plane})
		env.ExecuteWorkflow(DispatchOne, in)
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		attempts, _ := d.recorded()
		if len(attempts) != 1 || attempts[0].Number != 1 || attempts[0].IdentityAttempt() != identity {
			t.Fatalf("dispatch=%+v", attempts)
		}
		if physical > 0 {
			if err := refuseUnboundAttemptIdentity(DispatchOneWorkflowID(in.Envelope.RunID, "build", 1), in.Envelope, physical); err == nil {
				t.Fatal("old workflow identity accepted for physical reentry")
			}
		}
	}
}
