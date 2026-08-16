package readservice

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// TestDisableReadModelReadsForcesJournalPath is #2036's rollback test: before
// this issue, DisableReadModelReads had zero callers anywhere (not even a
// test), so the design's §6.6 promise — "rollback is a flag flip, never a
// deploy" — was API surface only. This pins that the toggle actually flips
// with a real ReadModel attached.
func TestDisableReadModelReadsForcesJournalPath(t *testing.T) {
	service := &Local{sources: LocalSources{ReadModel: brokenReader{}}}

	service.EnableReadModelReads()
	if !service.readModelReads {
		t.Fatal("read-model reads remain disabled after EnableReadModelReads")
	}

	service.DisableReadModelReads()
	if service.readModelReads {
		t.Fatal("read-model reads remain enabled after DisableReadModelReads")
	}
}

func TestReadModelRefusesUnsupportedFilterWithoutJournalFallback(t *testing.T) {
	ctx := context.Background()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	service, err := NewLocal(LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: testDefinitions(),
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.ListRuns(ctx, RunListOptions{Trigger: journal.TriggerSchedule})
	var unsupported *readmodel.UnsupportedCombinationError
	if !errors.As(err, &unsupported) {
		t.Fatalf("trigger-filtered list error = %v, want closed-set refusal", err)
	}
	if len(unsupported.Neighbours) == 0 {
		t.Fatal("trigger-filtered refusal does not name a supported neighbour")
	}
}

// TestReadModelPathHidesNoWorkByDefault is #2188's regression test for the
// read-model-served path specifically: listRunsFromReadModel must stay
// bounded and still hide no-work runs by default, since that is the common,
// unfiltered request every default portal view makes.
func TestReadModelPathHidesNoWorkByDefault(t *testing.T) {
	ctx := context.Background()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	identity := journal.RunIdentity{
		RunID: "run-no-work", Gaggle: "goobers", Workflow: "implementation",
		StartedAt: fixedTime,
	}
	events := []journal.Event{
		{Schema: journal.EventSchema, Seq: 1, Time: fixedTime.Add(time.Second),
			Type: journal.EventStageStarted, Stage: "implement"},
		{Schema: journal.EventSchema, Seq: 2, Time: fixedTime.Add(2 * time.Second),
			Type: journal.EventStageFinished, Stage: "implement", Status: "no-work"},
		{Schema: journal.EventSchema, Seq: 3, Time: fixedTime.Add(3 * time.Second),
			Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	}
	if err := store.UpsertRun(ctx, readmodel.ProjectRun(identity, readmodel.Projection{}, events)); err != nil {
		t.Fatal(err)
	}

	producedIdentity := journal.RunIdentity{
		RunID: "run-produced", Gaggle: "goobers", Workflow: "implementation",
		StartedAt: fixedTime.Add(time.Minute),
	}
	producedEvents := []journal.Event{
		{Schema: journal.EventSchema, Seq: 1, Time: fixedTime.Add(61 * time.Second),
			Type: journal.EventStageStarted, Stage: "implement"},
		{Schema: journal.EventSchema, Seq: 2, Time: fixedTime.Add(62 * time.Second),
			Type: journal.EventStageFinished, Stage: "implement", Status: "success"},
		{Schema: journal.EventSchema, Seq: 3, Time: fixedTime.Add(63 * time.Second),
			Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	}
	if err := store.UpsertRun(ctx, readmodel.ProjectRun(producedIdentity, readmodel.Projection{}, producedEvents)); err != nil {
		t.Fatal(err)
	}

	service, err := NewLocal(LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: testDefinitions(),
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	service.EnableReadModelReads()

	hidden, err := service.ListRuns(ctx, RunListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden.Runs) != 1 || hidden.Runs[0].ID != "run-produced" {
		t.Fatalf("default list from the read model = %+v, want only run-produced", hidden.Runs)
	}

	shown, err := service.ListRuns(ctx, RunListOptions{Limit: 50, ShowNoWork: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(shown.Runs) != 2 {
		t.Fatalf("ShowNoWork list from the read model = %+v, want both runs", shown.Runs)
	}
}

var fixedTime = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
