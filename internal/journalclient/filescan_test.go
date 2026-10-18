package journalclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// seedConflictRuns creates n runs in gaggle, each recording a base-sync
// conflict artifact. Runs whose index is below oldCount are aged: events.jsonl
// is backdated and then overwritten with garbage, so any scan that opens one
// fails loudly rather than quietly doing wasted work.
func seedConflictRuns(t testing.TB, layout instance.Layout, gaggle string, n, oldCount int, aged time.Time) {
	t.Helper()
	runsDir := layout.ForGaggle(gaggle).RunsDir()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		runID := fmt.Sprintf("run-%06d", i)
		at := now
		if i < oldCount {
			at = aged
		}
		run, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: gaggle},
			nil, journal.WithClock(func() time.Time { return at }))
		if err != nil {
			t.Fatalf("create %s: %v", runID, err)
		}
		data, _ := json.Marshal(conflictArtifact{Code: ConflictArtifactCode, ConflictingFiles: []string{"a.go"}})
		if _, err := run.RecordStageArtifact("local-ci", 1, "", "local-ci"+ConflictArtifactSuffix[0:], data); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if i < oldCount {
			events := filepath.Join(runsDir, runID, "events.jsonl")
			if err := os.WriteFile(events, []byte("not json\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(events, aged, aged); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestConflictTouchesPrunesOutOfWindowRunsWithoutOpeningJournals(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	aged := time.Now().UTC().Add(-90 * 24 * time.Hour)
	seedConflictRuns(t, layout, "web", 60, 50, aged)

	since := time.Now().UTC().Add(-30 * 24 * time.Hour)
	touches, err := NewFileCrossRun(layout).ConflictTouches(t.Context(), ConflictTouchRequest{Gaggle: "web", Since: since})
	if err != nil {
		t.Fatalf("scan opened a pruned (corrupt) journal: %v", err)
	}
	if len(touches) != 10 || touches[0].RunID != "run-000050" {
		t.Fatalf("touches = %d (first %v), want the 10 in-window runs", len(touches), touches)
	}
	// Without a window nothing is pruned, so the corrupt journals are read:
	// proof the fixtures would trip the scan if pruning did not skip them.
	if _, err := NewFileCrossRun(layout).ConflictTouches(t.Context(), ConflictTouchRequest{Gaggle: "web"}); err == nil {
		t.Fatal("unbounded scan did not read the backdated journals")
	}
}

func TestUnpushedWorkPrunesOutOfWindowRuns(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	aged := time.Now().UTC().Add(-90 * 24 * time.Hour)
	seedConflictRuns(t, layout, "web", 20, 20, aged)
	var warnings []string
	reader := NewFileCrossRun(layout)
	reader.Warn = func(m string) { warnings = append(warnings, m) }
	work, err := reader.UnpushedWork(t.Context(), UnpushedWorkRequest{
		Gaggle: "web", ItemIDs: []string{"x"}, Since: time.Now().UTC().Add(-30 * 24 * time.Hour),
	})
	if err != nil || work != nil || len(warnings) != 0 {
		t.Fatalf("work=%v err=%v warnings=%v, want none (no journal opened)", work, err, warnings)
	}
}

// BenchmarkConflictTouchesWindowed measures the scan over a large retained
// history where only a small tail is inside the window.
func BenchmarkConflictTouchesWindowed(b *testing.B) {
	layout := instance.NewLayout(b.TempDir())
	aged := time.Now().UTC().Add(-90 * 24 * time.Hour)
	seedConflictRuns(b, layout, "web", 300, 290, aged)
	since := time.Now().UTC().Add(-30 * 24 * time.Hour)
	reader := NewFileCrossRun(layout)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := reader.ConflictTouches(b.Context(), ConflictTouchRequest{Gaggle: "web", Since: since}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestWorkflowContributionPredicates(t *testing.T) {
	agentic := func(mode apiv1.WorkspaceMode) apiv1.Task {
		return apiv1.Task{Name: "a", Type: apiv1.TaskAgentic, Workspace: mode}
	}
	det := func(run apiv1.DeterministicRun) apiv1.Task {
		return apiv1.Task{Name: "d", Type: apiv1.TaskDeterministic, Run: &run}
	}
	for _, tc := range []struct {
		name             string
		spec             apiv1.WorkflowSpec
		conflict, strand bool
	}{
		{"empty", apiv1.WorkflowSpec{}, false, false},
		{"agentic default workspace", apiv1.WorkflowSpec{Tasks: []apiv1.Task{agentic("")}}, false, true},
		{"agentic repo", apiv1.WorkflowSpec{Tasks: []apiv1.Task{agentic(apiv1.WorkspaceRepo)}}, false, true},
		{"agentic read-only", apiv1.WorkflowSpec{Tasks: []apiv1.Task{agentic(apiv1.WorkspaceRepoReadOnly)}}, false, false},
		{"agentic scratch", apiv1.WorkflowSpec{Tasks: []apiv1.Task{agentic(apiv1.WorkspaceScratch)}}, false, false},
		{"deterministic syncBase", apiv1.WorkflowSpec{Tasks: []apiv1.Task{det(apiv1.DeterministicRun{SyncBase: true})}}, true, false},
		{"deterministic plain", apiv1.WorkflowSpec{Tasks: []apiv1.Task{det(apiv1.DeterministicRun{})}}, false, false},
		{"agentic gate only", apiv1.WorkflowSpec{Gates: []apiv1.Gate{{Name: "g", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Workspace: apiv1.WorkspaceRepo}}}}, false, false},
		{"both", apiv1.WorkflowSpec{Tasks: []apiv1.Task{agentic(""), det(apiv1.DeterministicRun{SyncBase: true})}}, true, true},
	} {
		if got := WorkflowCanRecordBaseSyncConflict(tc.spec); got != tc.conflict {
			t.Errorf("%s: conflict = %t, want %t", tc.name, got, tc.conflict)
		}
		if got := WorkflowCanStrandUnpushedWork(tc.spec); got != tc.strand {
			t.Errorf("%s: strand = %t, want %t", tc.name, got, tc.strand)
		}
	}
}
