package localscheduler

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
)

// WorkflowIdentity unambiguously identifies a workflow within its gaggle.
type WorkflowIdentity struct {
	Gaggle   string
	Workflow string
}

// ActiveRunCounts scans runsDir for running runs and returns per-workflow
// active counts — the daemon-startup reconciliation Conditions.Reconcile needs,
// since Conditions' in-memory counters don't survive a restart. Phase comes
// from the event log, the durable source of truth; state.json can lag a
// crash-fsynced run.finished event.
func ActiveRunCounts(runsDir string) (map[string]int, error) {
	scoped, _, err := activeRuns(runsDir)
	counts := map[string]int{}
	for identity, count := range scoped {
		counts[identity.Workflow] += count
	}
	return counts, err
}

// ActiveRunCountsByWorkflow returns active counts keyed by gaggle and workflow.
// Inventory readers use this projection because workflow names are only unique
// within a gaggle.
func ActiveRunCountsByWorkflow(runsDir string) (map[WorkflowIdentity]int, error) {
	counts, _, err := activeRuns(runsDir)
	return counts, err
}

func activeRuns(runsDir string) (map[WorkflowIdentity]int, map[string]WorkflowIdentity, error) {
	counts := map[WorkflowIdentity]int{}
	runs := map[string]WorkflowIdentity{}
	err := visitActiveRuns(runsDir, func(id journal.RunIdentity) {
		identity := WorkflowIdentity{Gaggle: id.Gaggle, Workflow: id.Workflow}
		counts[identity]++
		runs[id.RunID] = identity
	})
	return counts, runs, err
}

func visitActiveRuns(runsDir string, visit func(journal.RunIdentity)) error {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, e.Name())
		rd, err := journal.OpenRead(dir)
		if err != nil {
			continue // not a run directory
		}
		id, err := rd.Identity()
		if err != nil {
			continue
		}
		phase, err := rd.Phase()
		if err != nil {
			return fmt.Errorf("read phase for run %q: %w", id.RunID, err)
		}
		if phase == journal.PhaseRunning {
			visit(id)
		}
	}
	return nil
}
