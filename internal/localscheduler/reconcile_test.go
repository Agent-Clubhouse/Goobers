package localscheduler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestActiveRunCountsReconciliation(t *testing.T) {
	runsDir := t.TempDir()

	newRun := func(runID, workflow string, finish bool) {
		t.Helper()
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: runID, Workflow: workflow, WorkflowVersion: 1, Gaggle: "g",
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if finish {
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
		}
		_ = run.Close()
	}

	newRun("0af7651916cd43dd8448eb211c80319a", "implement", false) // active
	newRun("0af7651916cd43dd8448eb211c80319b", "implement", false) // active
	newRun("0af7651916cd43dd8448eb211c80319c", "implement", true)  // terminal, not counted
	newRun("0af7651916cd43dd8448eb211c80319d", "nominate", false)  // active, different workflow

	counts, err := ActiveRunCounts(runsDir)
	if err != nil {
		t.Fatalf("ActiveRunCounts: %v", err)
	}
	if counts["implement"] != 2 {
		t.Errorf("implement active count = %d, want 2", counts["implement"])
	}
	if counts["nominate"] != 1 {
		t.Errorf("nominate active count = %d, want 1", counts["nominate"])
	}
}

func TestActiveRunCountsMissingDir(t *testing.T) {
	counts, err := ActiveRunCounts(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing runs dir should not error (fresh instance): %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("expected empty counts, got %v", counts)
	}
}

func TestActiveRunCountsUsesEventLogPhase(t *testing.T) {
	for _, tc := range []struct {
		name             string
		corruptStateJSON bool
	}{
		{name: "stale running checkpoint"},
		{name: "unreadable checkpoint", corruptStateJSON: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runsDir := t.TempDir()
			const runID = "terminal-run"
			run, err := journal.Create(runsDir, journal.RunIdentity{
				RunID: runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
				Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(runsDir, runID, "state.json")
			staleState, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			if tc.corruptStateJSON {
				staleState = []byte("{")
			}
			if err := os.WriteFile(statePath, staleState, 0o644); err != nil {
				t.Fatal(err)
			}

			counts, err := ActiveRunCounts(runsDir)
			if err != nil {
				t.Fatalf("ActiveRunCounts: %v", err)
			}
			if got := counts["implement"]; got != 0 {
				t.Fatalf("active count = %d, want 0 for terminal event log", got)
			}
		})
	}
}
