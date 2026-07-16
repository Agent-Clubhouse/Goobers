package localscheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	runsDir := t.TempDir()

	for _, tc := range []struct {
		runID string
		state []byte
	}{
		{
			runID: "stale-terminal",
			state: mustJSON(t, journal.State{
				Schema: journal.StateSchema, RunID: "stale-terminal",
				Phase: journal.PhaseRunning, MachineState: "local-ci",
			}),
		},
		{runID: "unreadable-state", state: []byte("{not json")},
	} {
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: tc.runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runsDir, tc.runID, "state.json"), tc.state, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for restart := 0; restart < 3; restart++ {
		counts, err := ActiveRunCounts(runsDir)
		if err != nil {
			t.Fatalf("restart %d: ActiveRunCounts: %v", restart, err)
		}
		if got := counts["implement"]; got != 0 {
			t.Fatalf("restart %d: implement active count = %d, want 0", restart, got)
		}
	}
}

func TestActiveRunCountsSurfacesUnreadableEventLog(t *testing.T) {
	runsDir := t.TempDir()
	const runID = "corrupt-events"
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID, "events.jsonl"), []byte("{not json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ActiveRunCounts(runsDir)
	if err == nil {
		t.Fatal("ActiveRunCounts succeeded with an unreadable event log")
	}
	if !strings.Contains(err.Error(), runID) {
		t.Fatalf("error = %q, want run ID %q", err, runID)
	}
}

func TestReleaseReconciledOnlyReleasesMatchingRun(t *testing.T) {
	runsDir := t.TempDir()
	newRun := func(runID string, terminal bool) {
		t.Helper()
		run, err := journal.Create(runsDir, journal.RunIdentity{
			RunID: runID, Workflow: "implement", WorkflowVersion: 1, Gaggle: "g",
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if terminal {
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	newRun("running", false)
	newRun("terminal", true)

	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	sched := New(nil, log)
	if err := sched.Reconcile(runsDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := sched.conditions.Active("implement"); got != 1 {
		t.Fatalf("active count after reconcile = %d, want 1", got)
	}

	sched.ReleaseReconciled("terminal", "implement")
	if got := sched.conditions.Active("implement"); got != 1 {
		t.Fatalf("active count after terminal release = %d, want running run's slot preserved", got)
	}

	sched.ReleaseReconciled("running", "implement")
	if got := sched.conditions.Active("implement"); got != 0 {
		t.Fatalf("active count after running release = %d, want 0", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
