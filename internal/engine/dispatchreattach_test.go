package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestHandoffDoesNotChargeAttempt(t *testing.T) {
	in := runInput("handoff-same-attempt", apiv1.WorkflowSpec{
		Gaggle: "web", Start: "build", Tasks: []apiv1.Task{podTask("build", "", nil)},
	})
	in.Placements = []PinnedPlacement{remotePin("build")}
	in.Spec.Tasks[0].PolicyActions = []string{"comment-on-pr-advisory"}
	in.Spec.Tasks[0].Capabilities = []string{"github:pr:write"}
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true}
	var inputs []DispatchStageInput
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{})
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, input DispatchStageInput) (stageActivityResult, error) {
		inputs = append(inputs, input)
		if len(inputs) == 1 {
			return stageActivityResult{}, dispatchWorkerHandoff(custody)
		}
		return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0].Envelope.Attempt != 1 || inputs[1].Envelope.Attempt != 1 ||
		inputs[1].Custody == nil || inputs[1].Custody.UID != custody.UID ||
		inputs[0].Class != "" || inputs[1].Class != "" {
		t.Fatalf("handoff dispatches = %+v", inputs)
	}
}

func TestReattachPreservesAttemptDeadline(t *testing.T) {
	var first, second time.Time
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "queue")
		deadline := workflow.Now(ctx).Add(time.Minute)
		firstCtx := dispatchReattachActivityContext(ctx, deadline.Sub(workflow.Now(ctx)))
		first = workflow.Now(ctx).Add(workflow.GetActivityOptions(firstCtx).StartToCloseTimeout)
		if err := workflow.Sleep(ctx, 10*time.Second); err != nil {
			return err
		}
		secondCtx := dispatchReattachActivityContext(ctx, deadline.Sub(workflow.Now(ctx)))
		second = workflow.Now(ctx).Add(workflow.GetActivityOptions(secondCtx).StartToCloseTimeout)
		return nil
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatalf("activity deadline reset across handoff: first=%s second=%s", first, second)
	}
}

func TestHandoffCapIsInfra(t *testing.T) {
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true}
	calls := 0
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{})
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, _ DispatchStageInput) (stageActivityResult, error) {
		calls++
		return stageActivityResult{}, dispatchWorkerHandoff(custody)
	})
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "queue")
		_, err := executeDispatchActivity(ctx, dispatchInput("handoff-cap", "build", 1))
		return err
	})
	err := env.GetWorkflowError()
	if err == nil || calls != maxDispatchHandoffs+1 || !strings.Contains(err.Error(), "handoff limit") {
		t.Fatalf("handoff cap: calls=%d err=%v", calls, err)
	}
	class, classifyErr := ClassifyDispatchFailure(err)
	if classifyErr != nil || class != journal.AttemptInfra {
		t.Fatalf("handoff exhaustion class = %q, %v", class, classifyErr)
	}
}

func TestDispatchReattachVersionGateReplaysPreChangeHistory(t *testing.T) {
	data, err := os.ReadFile("testdata/dispatchstage_history.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), dispatchReattachChange) {
		t.Fatal("pre-change fixture already contains the reattach marker")
	}
	history := &historypb.History{}
	if err := protojson.Unmarshal(data, history); err != nil {
		t.Fatal(err)
	}
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("pre-reattach history does not replay through %s: %v", dispatchReattachChange, err)
	}
}

func TestDispatchHandoffDetailsRequireCustody(t *testing.T) {
	for _, err := range []error{
		temporal.NewApplicationError("worker-handoff", failureTypeWorkerHandoff),
		dispatchWorkerHandoff(dispatcher.PodCustody{}),
	} {
		if _, ok := dispatchHandoffCustody(err); ok {
			t.Fatalf("invalid custody accepted: %v", err)
		}
	}
	if _, ok := dispatchHandoffCustody(errors.New("ordinary failure")); ok {
		t.Fatal("ordinary failure classified as handoff")
	}
}
