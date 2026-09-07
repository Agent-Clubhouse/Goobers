package readservice

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestActiveStageTimingMatchesJournalAndSQLiteAcrossRetryAndCompletion(t *testing.T) {
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	machine := fixtureMachine(t)
	run, clock := createFixtureRun(t, layout, machine, "active-run", machine.Def.Name, machine.Def.Spec.Gaggle, fixedTime, journal.Trigger{Kind: journal.TriggerManual}, false)
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions(), ReadModel: store}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	check := func(want []readmodel.ActiveStage) {
		t.Helper()
		if err := store.ProjectRunDir(ctx, filepath.Join(layout.RunsDir(), "active-run")); err != nil {
			t.Fatal(err)
		}
		service.EnableReadModelReads()
		projected, err := service.ListStatusRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		service.DisableReadModelReads()
		authoritative, err := service.ListStatusRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(projected) != 1 || len(authoritative) != 1 {
			t.Fatalf("run counts %d/%d", len(projected), len(authoritative))
		}
		if !reflect.DeepEqual(projected[0].ActiveStages, authoritative[0].ActiveStages) || !reflect.DeepEqual(projected[0].ActiveStages, want) {
			t.Fatalf("activity: projected=%+v journal=%+v want=%+v", projected[0].ActiveStages, authoritative[0].ActiveStages, want)
		}
	}
	for attempt := 1; attempt <= 2; attempt++ {
		clock.now = fixedTime.Add(time.Duration(attempt) * time.Minute)
		if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: attempt, Runner: map[string]any{"goober": "original-owner"}}); err != nil {
			t.Fatal(err)
		}
		check([]readmodel.ActiveStage{{Name: "implement", Kind: "stage", Attempt: attempt, Goober: "original-owner", StartedAt: clock.now}})
		if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: attempt, Status: "failure"}); err != nil {
			t.Fatal(err)
		}
	}
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)
	check(nil)
}
