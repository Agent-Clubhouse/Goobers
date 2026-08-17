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
	"github.com/goobers/goobers/internal/telemetry/rollup"
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
	if service.ReadMode() != ReadModeAuthoritative {
		t.Fatalf("read mode = %s, want authoritative", service.ReadMode())
	}
}

func TestDisableReadModelReadsListsJournalsWhenRollupIsEmptyAfterRestart(t *testing.T) {
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	machine := fixtureMachine(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-before-restart", machine.Def.Name, machine.Def.Spec.Gaggle,
		fixedTime, journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)

	telemetry, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = telemetry.Close() })
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
		Telemetry:   telemetry,
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	service.DisableReadModelReads()

	for _, options := range []RunListOptions{{Limit: 50}, {LatestPerWorkflow: true}} {
		page, err := service.ListRuns(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != 1 || page.Runs[0].ID != "run-before-restart" {
			t.Fatalf("rollback list with options %+v = %+v, want run-before-restart", options, page.Runs)
		}
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

func TestReadModelPathHydratesOperatorSummaryFromJournal(t *testing.T) {
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	machine := fixtureMachine(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"projected-operator",
		"implementation",
		"goobers",
		fixedTime,
		journal.Trigger{Kind: journal.TriggerItem, Ref: "3088"},
		true,
	)
	clock.now = fixedTime.Add(time.Minute)
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implementation"}); err != nil {
		t.Fatal(err)
	}
	clock.now = fixedTime.Add(2 * time.Minute)
	if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "implementation"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity := journal.RunIdentity{
		RunID:     "projected-operator",
		Gaggle:    "goobers",
		Workflow:  "implementation",
		StartedAt: fixedTime,
		Trigger:   journal.Trigger{Kind: journal.TriggerItem, Ref: "3088"},
	}
	events := []journal.Event{
		{Schema: journal.EventSchema, Seq: 1, Time: fixedTime.Add(time.Minute), Type: journal.EventStageStarted, Stage: "implementation"},
		{Schema: journal.EventSchema, Seq: 2, Time: fixedTime.Add(2 * time.Minute), Type: journal.EventStageHeartbeat, Stage: "implementation"},
	}
	if err := store.UpsertRun(ctx, readmodel.ProjectRun(identity, readmodel.Projection{}, events)); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
		ReadModel:   store,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return fixedTime.Add(3 * time.Minute) }

	page, err := service.ListRuns(ctx, RunListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("runs = %+v, want one", page.Runs)
	}
	operator := page.Runs[0].Operator
	if operator.Issue == nil || operator.Issue.Number != "3088" ||
		operator.CurrentStage != "implementation" ||
		operator.HeartbeatAgeMillis == nil ||
		*operator.HeartbeatAgeMillis != time.Minute.Milliseconds() {
		t.Fatalf("operator = %+v", operator)
	}
}

var fixedTime = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
