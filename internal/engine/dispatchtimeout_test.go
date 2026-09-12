package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// A missed dispatch heartbeat is an uncertain execution, not proof that no
// effect happened. The same typed history failure must drive both retry and
// journal projection as policy, even when its nested cause mentions infra.
func TestDispatchHeartbeatTimeoutProjectsPolicyAndStopsMutation(t *testing.T) {
	in := runInput("heartbeat-mutation", apiv1.WorkflowSpec{
		Gaggle: "web", Start: "build", Tasks: []apiv1.Task{podTask("build", "", nil)},
	})
	in.Spec.Tasks[0].Capabilities = []string{"repo:push"}
	in.Spec.Tasks[0].PolicyActions = []string{"modify-repository"}
	in.Spec.Tasks[0].Retry = &apiv1.RetryPolicy{MaxAttempts: 3}
	in.Placements = []PinnedPlacement{remotePin("build")}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t)})
	timeout := temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, temporal.NewApplicationError("supervisor lost", FailureTypeInfrastructure))
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(stageActivityResult{}, timeout).Once()
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), "refusing to retry after worker loss") {
		t.Fatalf("uncertain mutation result = %v", err)
	}
	value, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatal(err)
	}
	var projection JournalProjection
	if err := value.Get(&projection); err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, op := range projection.Ops {
		if op.Event == nil || op.Event.Type != journal.EventError || op.Event.Error == nil {
			continue
		}
		if op.Event.Error.Code != "executor_error" && op.Event.Error.Code != "run_failed" {
			continue
		}
		failures++
		if op.Event.Runner["errorClass"] != "executor" || op.Event.Runner["retryFailureClass"] != string(journal.AttemptPolicy) {
			t.Fatalf("heartbeat failure projection = %+v", op.Event)
		}
	}
	if failures != 2 {
		t.Fatalf("projected attempt/terminal failures = %d, want 2", failures)
	}
	env.AssertExpectations(t)
}
