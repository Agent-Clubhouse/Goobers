package engine

import (
	"fmt"
	"os"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"
)

// Captured from production Run on main at 77f14d080, before engine human-gate
// support: a human-gate workflow with an enabled HITL policy refused at run
// start, with no version marker. The enabled Run must replay that refusal in
// full and at every workflow-task boundary.
func TestHumanGatePreSupportRefusalHistoryReplays(t *testing.T) {
	data, err := os.ReadFile("testdata/human-gate-refusal-n1.json")
	if err != nil {
		t.Fatal(err)
	}
	var history historypb.History
	if err := protojson.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	for _, event := range history.Events {
		if event.GetMarkerRecordedEventAttributes() != nil {
			t.Fatal("N-1 fixture unexpectedly records a version marker")
		}
	}
	replay := func(t *testing.T, h *historypb.History) {
		t.Helper()
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflow(Run)
		execution := workflow.Execution{ID: "human-gate-n1", RunID: h.Events[0].GetWorkflowExecutionStartedEventAttributes().GetOriginalExecutionRunId()}
		if err := replayer.ReplayWorkflowHistoryWithOptions(nil, h, temporalworker.ReplayWorkflowHistoryOptions{OriginalExecution: execution}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("full", func(t *testing.T) { replay(t, &history) })
	for i, event := range history.Events {
		if event.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			t.Run(fmt.Sprintf("workflow-task-%d", event.GetEventId()), func(t *testing.T) {
				replay(t, &historypb.History{Events: history.Events[:i+1]})
			})
		}
	}
}
