package engine

import (
	"fmt"
	"os"
	"testing"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
)

// Captured on 2661f0209, before production Run assembled the controller.
// The refused history reaches a static parallel and fails with unknown-state;
// the serial history completes. Worker/sticky identities are sanitized, with
// recorded commands and payloads unchanged. Both must retain their old path.
func TestProductionRunPreFanOutHistoriesReplay(t *testing.T) {
	for _, kind := range []string{"serial", "refused"} {
		t.Run(kind, func(t *testing.T) {
			data, err := os.ReadFile("testdata/run-fanout-pre-assembly-" + kind + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var history historypb.History
			if err := protojson.Unmarshal(data, &history); err != nil {
				t.Fatal(err)
			}
			replay := func(t *testing.T, h *historypb.History) {
				t.Helper()
				r := worker.NewWorkflowReplayer()
				r.RegisterWorkflow(Run)
				if err := r.ReplayWorkflowHistory(nil, h); err != nil {
					t.Fatal(err)
				}
			}
			t.Run("full", func(t *testing.T) { replay(t, &history) })
			for i, event := range history.Events {
				if event.GetWorkflowTaskStartedEventAttributes() != nil {
					t.Run(fmt.Sprintf("workflow-task-%d", event.GetEventId()), func(t *testing.T) { replay(t, &historypb.History{Events: history.Events[:i+1]}) })
				}
			}
		})
	}
}
