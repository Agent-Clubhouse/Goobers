package harness

import (
	"context"
	"os"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestActualAdapterDeadlineNormalizesProductionTaskIdentity(t *testing.T) {
	recorder := &fakeRecorder{}
	adapter := &FakeAdapter{Act: func(ctx context.Context, _ RunRequest) error {
		_, err := (ExecProcessRunner{}).Run(ctx, ProcessRequest{Command: []string{os.Args[0], "-test.run=^TestDeadlineProcessHelper$", "--", "deadline-helper"}, Timeout: time.Minute})
		return err
	}}
	executor := &Executor{adapter: adapter, recorder: recorder}
	now := time.Now().UTC()
	_, err := executor.runAdapter(context.Background(), RunRequest{Envelope: apiv1.InvocationEnvelope{RunID: "run-id", TaskID: "run-id:work"}, Attempt: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	activity := (readmodel.StageActivity{}).After(journal.Event{Schema: journal.EventSchema, Type: journal.EventStageStarted, Stage: "work", Attempt: 1, Time: now})
	found := false
	for _, event := range recorder.events {
		if event.Runner["kind"] != executiondeadline.Kind {
			continue
		}
		if event.Stage != "work" {
			t.Fatal("deadline retained run-prefixed task ID", event)
		}
		event.Schema = journal.EventSchema
		activity = activity.After(event)
		if event.Runner["executionState"] == "active" && activity.Active[0].ExecutionDeadline != nil {
			found = true
		}
	}
	if !found || activity.Active[0].ExecutionDeadline != nil {
		t.Fatal("actual executor evidence did not attach and clear current bare-name stage")
	}
}
