package executor

import (
	"context"
	"os"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/journal"
)

func TestShellExecutionDeadlineUsesActualContext(t *testing.T) {
	executor, recorder := newPortableTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.RunID, env.TaskID, env.Attempt = "run-id", "run-id:work", 1
	deadline := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_, err := executor.Run(ctx, env, apiv1.DeterministicRun{Command: []string{os.Args[0], "-test.run=^TestFeatureProviderProcessHelper$"}})
	if err != nil {
		t.Fatal(err)
	}
	var events []journal.Event
	for _, event := range recorder.events {
		if event.Runner["kind"] == executiondeadline.Kind {
			events = append(events, event)
		}
	}
	if len(events) != 2 {
		t.Fatalf("execution boundaries=%+v", events)
	}
	actual, err := time.Parse(time.RFC3339Nano, events[0].Runner["deadline"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Stage != "work" || !actual.Equal(deadline) || events[0].Runner["executionState"] != "active" || events[1].Runner["executionState"] != "finished" {
		t.Fatal(events)
	}
}
