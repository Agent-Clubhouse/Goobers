package engine

import (
	"testing"

	historypb "go.temporal.io/api/history/v1"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func replayGateDecisionHistory(t *testing.T, history *historypb.History) {
	t.Helper()
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(runHumanGateTestWorkflow, workflow.RegisterOptions{Name: "Run"})
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
}
