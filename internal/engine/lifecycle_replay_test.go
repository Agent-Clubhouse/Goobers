package engine

import (
	"fmt"
	"os"
	"testing"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
)

// Captured from the bounded staticScheduler on 2661f0209, before lifecycle
// receipt requests existed. Worker identities are sanitized; recorded commands
// and activity results are unchanged. In particular, EmitJournal has no receipts.
func TestStaticSchedulerPreReceiptHistoryReplays(t *testing.T) {
	data, err := os.ReadFile("testdata/static-scheduler-pre-receipts.json")
	if err != nil {
		t.Fatal(err)
	}
	var history historypb.History
	if err := protojson.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	replay := func(t *testing.T, history *historypb.History) {
		t.Helper()
		r := worker.NewWorkflowReplayer()
		r.RegisterWorkflow(staticScheduler)
		if err := r.ReplayWorkflowHistory(nil, history); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("full", func(t *testing.T) { replay(t, &history) })
	for i, event := range history.Events {
		if event.GetWorkflowTaskStartedEventAttributes() != nil {
			t.Run(fmt.Sprintf("workflow-task-%d", event.GetEventId()), func(t *testing.T) {
				replay(t, &historypb.History{Events: history.Events[:i+1]})
			})
		}
	}
}
