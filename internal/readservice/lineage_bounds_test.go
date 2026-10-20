package readservice

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/runlineage"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestOfflineLineageDoesNotOpenUnrelatedJournals(t *testing.T) {
	service, layout, machine := fixtureService(t)
	for i := 0; i < 100; i++ {
		run, clock := createFixtureRun(t, layout, machine, fmt.Sprintf("unrelated-%03d", i), machine.Def.Name, "goobers", time.Now(), journal.Trigger{Kind: journal.TriggerManual}, true)
		finishFixtureRun(t, run, clock, journal.PhaseCompleted)
	}
	opens := 0
	openRunObserver = func(string) { opens++ }
	t.Cleanup(func() { openRunObserver = nil })
	if _, err := service.offlineRunLineage(context.Background(), RunSummary{ID: "target"}); err != nil {
		t.Fatal(err)
	}
	if opens != 0 {
		t.Fatalf("lineage opened %d unrelated journals; want zero", opens)
	}
}

type countingLineageReader struct {
	readmodel.Reader
	calls  int
	cancel context.CancelFunc
}

func (r *countingLineageReader) GetRun(_ context.Context, id string) (readmodel.RunRow, bool, error) {
	r.calls++
	if r.cancel != nil {
		r.cancel()
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "ancestor-"))
	row := readmodel.RunRow{RunID: id, Phase: journal.PhaseCompleted, RepassCount: 1}
	row.Operator.ContinuedFromRunID = fmt.Sprintf("ancestor-%d", n+1)
	return row, true, nil
}
func (r *countingLineageReader) ContinuationRuns(context.Context, string, string) (runlineage.Page, error) {
	return runlineage.Page{}, nil
}

func TestProjectedLineageBoundsAndCachesSharedAncestors(t *testing.T) {
	reader := &countingLineageReader{}
	service := &Local{sources: LocalSources{ReadModel: reader}, readModelReads: true}
	runs := []RunSummary{{ID: "target-a", Lineage: &RunLineage{Source: &LineageRun{ID: "ancestor-0"}}}, {ID: "target-b", Lineage: &RunLineage{Source: &LineageRun{ID: "ancestor-0"}}}}
	if err := service.decorateRunLineage(t.Context(), runs); err != nil {
		t.Fatal(err)
	}
	if reader.calls != runlineage.MaxDepth {
		t.Fatalf("ancestor lookups=%d", reader.calls)
	}
	for _, run := range runs {
		if run.Lineage.HistoricalRepassCount != runlineage.MaxDepth || !run.Lineage.HistoricalRepassCountPartial {
			t.Fatalf("unmarked partial history: %+v", run.Lineage)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader.calls = 0
	reader.cancel = cancel
	if err := service.decorateRunLineage(ctx, runs); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("lookups after cancellation=%d", reader.calls)
	}
}

func TestSummaryLineageCyclesExcludeTheTarget(t *testing.T) {
	runs := []RunSummary{
		{ID: "a", RepassCount: 10, Lineage: &RunLineage{Source: &LineageRun{ID: "b"}}},
		{ID: "b", RepassCount: 20, Lineage: &RunLineage{Source: &LineageRun{ID: "a"}}},
	}
	if err := decorateRunLineageFromSummaries(t.Context(), runs); err != nil {
		t.Fatal(err)
	}
	if runs[0].Lineage.HistoricalRepassCount != 20 || runs[1].Lineage.HistoricalRepassCount != 10 {
		t.Fatalf("cycle counted own budget: %+v %+v", runs[0].Lineage, runs[1].Lineage)
	}
	for _, run := range runs {
		if !run.Lineage.HistoricalRepassCountPartial {
			t.Fatal("cycle was not reported")
		}
	}
}

func TestOfflineLineageBoundsAncestorJournalOpens(t *testing.T) {
	service, layout, _ := fixtureService(t)
	for i := 0; i < runlineage.MaxDepth+5; i++ {
		parent := fmt.Sprintf("ancestor-%d", i+1)
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: fmt.Sprintf("ancestor-%d", i), Workflow: "wf", ContinuedFromRunID: parent}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	opens := 0
	openRunObserver = func(string) { opens++ }
	t.Cleanup(func() { openRunObserver = nil })
	lineage, err := service.offlineRunLineage(t.Context(), RunSummary{ID: "target", Lineage: &RunLineage{Source: &LineageRun{ID: "ancestor-0"}}})
	if err != nil {
		t.Fatal(err)
	}
	if opens != runlineage.MaxDepth || !lineage.HistoricalRepassCountPartial {
		t.Fatalf("opens=%d lineage=%+v", opens, lineage)
	}
}

func TestOfflineLineageUsesOnDiskRollup(t *testing.T) {
	service, layout, _ := fixtureService(t)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: "child", Workflow: "wf", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "source"}, ContinuedFromRunID: "source"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.IngestRun(t.Context(), filepath.Join(layout.RunsDir(), "child")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opens := 0
	openRunObserver = func(string) { opens++ }
	t.Cleanup(func() { openRunObserver = nil })
	lineage, err := service.offlineRunLineage(t.Context(), RunSummary{ID: "source"})
	if err != nil {
		t.Fatal(err)
	}
	if lineage == nil || len(lineage.Continuations) != 1 || lineage.Continuations[0].ID != "child" || lineage.ContinuationsIncomplete || opens != 0 {
		t.Fatalf("disk lookup opens=%d lineage=%+v", opens, lineage)
	}
	lineage, err = service.offlineRunLineagePage(t.Context(), RunSummary{ID: "source"}, "child")
	if err != nil {
		t.Fatal(err)
	}
	if lineage != nil && len(lineage.Continuations) > 0 {
		t.Fatalf("cursor ignored: %+v", lineage)
	}
}
