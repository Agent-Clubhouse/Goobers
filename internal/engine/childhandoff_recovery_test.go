package engine

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// A heartbeat timeout can hide a committed Finish/revoke and a stopped pod.
// Reacquisition of that same grant then returns the production refusal class.
func TestProductionRunLostChildHandoffCompletionNeverRedispatches(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)

	writer, _ := newLiveWriter(t)
	env.RegisterActivity(&Activities{Journal: writer})
	var inputs []DispatchStageInput
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, input DispatchStageInput) (stageActivityResult, error) {
		inputs = append(inputs, input)
		switch len(inputs) {
		case 1:
			return stageActivityResult{}, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil, dispatchHeartbeatDetails{Namespace: "gaggle-web", Pod: "parent", UID: "uid-1", Attempt: 1, Custody: true})
		case 2:
			// stageChildAuthority classifies a revoked same-attempt binding this way.
			return stageActivityResult{}, temporal.NewNonRetryableApplicationError("grant revoked during lost Finish", childCustodyRefusedType, ErrChildCustodyRefused)
		default:
			return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
		}
	})
	in := childParentInput()
	in.ChildWorkflowsEnabled = true
	in.Placements = []PinnedPlacement{remotePin("parent")}
	env.ExecuteWorkflow(Run, in)
	for _, input := range inputs {
		if input.Envelope.Attempt > 1 {
			t.Fatalf("fresh stage attempt after lost child handoff: calls=%d attempt=%d custody=%+v workflowErr=%v", len(inputs), input.Envelope.Attempt, input.Custody, env.GetWorkflowError())
		}
	}
	if len(inputs) != 2 {
		t.Fatalf("wanted initial dispatch and reattach; got %d (error %v)", len(inputs), env.GetWorkflowError())
	}
}

func TestDispatchStageRecoversCompletedHandoffWithoutDispatch(t *testing.T) {
	input := handoffInput()
	handoff := acceptedHandoff(input)
	authority := &handoffAuthority{fakeChildAuthority: fakeChildAuthority{grant: liveGrant()}}
	authority.observe = func(_ context.Context, request ChildHandoffObservation) (*journal.ChildHandoffRequest, error) {
		if request.Finish {
			if !request.Stopped {
				t.Fatal("completion sealed without stop proof")
			}
			authority.grant.StoppedHandoff = &handoff
		}
		return &handoff, nil
	}
	dispatches := 0
	a := &Activities{ChildAuthority: authority, Surrenders: surrenderStore(t), Dispatcher: handoffDispatcher{dispatch: func(context.Context, dispatcher.Attempt) (dispatcher.Report, error) {
		dispatches++
		return dispatcher.Report{ParentWritersStopped: true}, nil
	}}}
	result, err := a.DispatchStage(t.Context(), input)
	if err != nil || result.ChildWait == nil {
		t.Fatalf("first dispatch: %+v %v", result, err)
	}
	// Completion can be lost before heartbeat custody is available too.
	for _, custody := range []*dispatcher.PodCustody{nil, {Namespace: "gaggle-web", Pod: "parent", UID: "uid-1", Attempt: 1, Custody: true}} {
		input.Custody = custody
		result, err = a.DispatchStage(t.Context(), input)
		if err != nil || result.ChildWait == nil || *result.ChildWait != handoff || dispatches != 1 {
			t.Fatalf("recovery result=%+v err=%v dispatches=%d", result, err, dispatches)
		}
	}
}

func TestStageChildAuthorityAmbiguousReacquisitionStops(t *testing.T) {
	for _, err := range []error{ErrChildCustodyRefused, ErrChildCustodyUnavailable} {
		input := handoffInput()
		input.Custody = &dispatcher.PodCustody{UID: "known-pod", Custody: true}
		authority := &fakeChildAuthority{acquireErr: err}
		var attempt dispatcher.Attempt
		if _, got := (&Activities{ChildAuthority: authority}).stageChildAuthority(t.Context(), input, &attempt); !isChildHandoffFailure(got) {
			t.Fatalf("ambiguous reacquire became retry: %v", got)
		}
	}
}
