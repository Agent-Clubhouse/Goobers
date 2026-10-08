package readservice

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// TestDisplayTitleConsistentAcrossListAndDetail pins #5429: the display title
// a claim stage publishes is returned identically by the read-model list, the
// journal-backed list, and run detail; a run that never published one keeps
// only its claimed-issue fallback.
func TestDisplayTitleConsistentAcrossListAndDetail(t *testing.T) {
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	machine := fixtureMachine(t)
	const canonical = "Investigate #123456789: Example incident title"
	runs := map[string]string{"titled-run": canonical, "untitled-run": ""}
	for runID, title := range runs {
		run, clock := createFixtureRun(t, layout, machine, runID, machine.Def.Name, machine.Def.Spec.Gaggle,
			fixedTime, journal.Trigger{Kind: journal.TriggerManual}, false)
		outputs := map[string]any{"id": "123456789", "title": "Example incident title"}
		if title != "" {
			if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "claim", Status: "failure",
				Outputs: map[string]any{readmodel.OutputDisplayTitle: "Failed attempt title"}}); err != nil {
				t.Fatal(err)
			}
			outputs[readmodel.OutputDisplayTitle] = title
		}
		if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "claim", Status: "success",
			Outputs: outputs}); err != nil {
			t.Fatal(err)
		}
		finishFixtureRun(t, run, clock, journal.PhaseCompleted)
	}
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for runID := range runs {
		if err := store.ProjectRunDir(ctx, filepath.Join(layout.RunsDir(), runID)); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions(), ReadModel: store}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	check := func(source string, operator OperatorRunSummary, runID string) {
		t.Helper()
		if operator.DisplayTitle != runs[runID] {
			t.Errorf("%s %s display title = %q, want %q", source, runID, operator.DisplayTitle, runs[runID])
		}
		if operator.Issue == nil || operator.Issue.Number != "123456789" || operator.Issue.Title != "Example incident title" {
			t.Errorf("%s %s issue fallback = %+v", source, runID, operator.Issue)
		}
	}
	for _, readModel := range []bool{true, false} {
		if readModel {
			service.EnableReadModelReads()
		} else {
			service.DisableReadModelReads()
		}
		page, err := service.ListRuns(ctx, RunListOptions{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) != len(runs) {
			t.Fatalf("list (readModel=%t) returned %d runs, want %d", readModel, len(page.Runs), len(runs))
		}
		for _, summary := range page.Runs {
			check(map[bool]string{true: "read-model list", false: "journal list"}[readModel], summary.Operator, summary.ID)
		}
		for runID := range runs {
			detail, err := service.GetRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			check("detail", detail.Operator, runID)
		}
	}
}
