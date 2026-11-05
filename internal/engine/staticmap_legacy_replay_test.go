package engine

import (
	"os"
	"testing"

	historypb "go.temporal.io/api/history/v1"
	temporalworker "go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestLegacyMapRefusalHistoryReplaysAtEveryWorkflowTaskStart(t *testing.T) {
	data, err := os.ReadFile("testdata/legacy-map-refused-history.json")
	if err != nil {
		t.Fatal(err)
	}
	var history historypb.History
	if err := protojson.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	markers, activities := 0, 0
	for _, event := range history.Events {
		if event.GetMarkerRecordedEventAttributes() != nil {
			markers++
		}
		if event.GetActivityTaskScheduledEventAttributes() != nil {
			activities++
		}
	}
	if len(history.Events) != 5 || markers != 0 || activities != 0 {
		t.Fatalf("legacy refusal history has %d events, %d markers, %d activities", len(history.Events), markers, activities)
	}
	replay := func(t *testing.T, recorded *historypb.History) {
		t.Helper()
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflow(Run)
		if err := replayer.ReplayWorkflowHistory(nil, recorded); err != nil {
			t.Fatal(err)
		}
	}
	replay(t, &history)
	prefixes := 0
	for i, event := range history.Events {
		if event.GetWorkflowTaskStartedEventAttributes() == nil {
			continue
		}
		prefixes++
		replay(t, &historypb.History{Events: history.Events[:i+1]})
	}
	if prefixes != 1 {
		t.Fatalf("legacy refusal history has %d workflow-task prefixes, want 1", prefixes)
	}
}
