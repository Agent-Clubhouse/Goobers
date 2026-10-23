package engine

import (
	"testing"

	historypb "go.temporal.io/api/history/v1"
	temporalworker "go.temporal.io/sdk/worker"
)

func replayGateDecisionHistory(t *testing.T, history *historypb.History) {
	t.Helper()
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
}
