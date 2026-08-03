package readservice

import (
	"context"
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
// what readModelEligible answers, with a real ReadModel attached so the
// difference is provably the flag, not the store's absence.
func TestDisableReadModelReadsForcesJournalPath(t *testing.T) {
	service := &Local{sources: LocalSources{ReadModel: brokenReader{}}}
	options := RunListOptions{Limit: 50}

	service.EnableReadModelReads()
	if !service.readModelEligible(options) {
		t.Fatal("readModelEligible() = false with reads enabled and a store attached, want true")
	}

	service.DisableReadModelReads()
	if service.readModelEligible(options) {
		t.Fatal("readModelEligible() = true after DisableReadModelReads(), want false — " +
			"the rollback must force every list request onto the journal-derived paths")
	}
}

// TestReadModelPathHidesNoWorkByDefault is #2188's regression test for the
// read-model-served path specifically: listRunsFromReadModel must stay
// eligible (no forced fallback) and still hide no-work runs by default, since
// that is the common, unfiltered request every default portal view makes.
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

	options := RunListOptions{Limit: 50}
	if !service.readModelEligible(options) {
		t.Fatal("readModelEligible() = false with reads enabled and a store attached, want true — " +
			"hiding no-work must not force the journal-derived fallback for the default view")
	}

	hidden, err := service.ListRuns(ctx, options)
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
