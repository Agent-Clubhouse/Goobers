package runner

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestRunnerPersistsEngineFallbackBeforeStageExecution(t *testing.T) {
	const runID = "fallback-run"
	r, dir := newTestRunner(t, map[string]stubTaskResult{runID + ":query-backlog": {status: apiv1.ResultNoWork}}, nil)
	machine := noWorkFixtureMachine(t)
	result, err := r.Start(context.Background(), StartInput{RunID: runID, Machine: machine, Gaggle: "acme-web",
		StarterSelection: map[string]any{"kind": journal.RunnerAnnotationEngineSelection, "reasonClass": "placement_ineligible", "reason": "self-pinned", "selfPinnedStages": []string{"query-backlog"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseCompleted || !result.NoWork {
		t.Fatalf("routing outcome changed: %+v", result)
	}
	reader, err := journal.OpenRead(filepath.Join(dir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, event := range events {
		if value, ok := readmodel.RunnerEngineFallback(event); ok {
			seen = true
			if value.RunID != runID || value.Workflow != machine.Def.Name || value.Gaggle != "acme-web" {
				t.Fatalf("lost identity: %+v", value)
			}
		}
		if event.Type == journal.EventStageStarted && !seen {
			t.Fatal("stage started before durable routing evidence")
		}
	}
	if !seen {
		t.Fatal("no run-level routing evidence")
	}
}
