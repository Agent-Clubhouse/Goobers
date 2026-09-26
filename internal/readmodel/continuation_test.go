package readmodel

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestContinuationRunsReturnsDirectChildren(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for _, identity := range []journal.RunIdentity{
		{RunID: "source", Gaggle: "g", Workflow: "wf"},
		{RunID: "child-b", Gaggle: "g", Workflow: "wf", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "source"}, ContinuedFromRunID: "source"},
		{RunID: "child-a", Gaggle: "g", Workflow: "wf", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "source"}, ContinuedFromRunID: "source"},
	} {
		projection := ProjectRun(identity, Projection{}, completedRunEvents())
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ContinuationRuns(ctx, []string{"source"})
	if err != nil {
		t.Fatal(err)
	}
	children := got["source"]
	if len(children) != 2 || children[0].RunID != "child-a" || children[1].RunID != "child-b" {
		t.Fatalf("continuations = %+v", children)
	}
}
