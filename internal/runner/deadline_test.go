package runner

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestLocalParallelDeadlineUsesProductionBranchRecorder(t *testing.T) {
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "run", Gaggle: "g", Workflow: "w"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	recorder := &branchJournal{run: run, branch: 3}
	if err := recorder.Append(journal.Event{Type: journal.EventStageStarted, Stage: "work", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = executiondeadline.WithRecorder(ctx, recorder, "work", 1)
	_, done := invoke.BeginExecution(ctx)
	defer done()
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var activity readmodel.StageActivity
	for _, event := range events {
		activity = activity.After(event)
	}
	if len(activity.Active) != 1 || activity.Active[0].Branch != 3 || activity.Active[0].ExecutionDeadline == nil {
		t.Fatalf("parallel branch evidence absent: %+v", activity)
	}
}
