package localscheduler

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
)

// ActiveRunCounts scans runsDir for non-terminal runs and returns per-workflow
// active counts — the daemon-startup reconciliation Conditions.Reconcile needs,
// since Conditions' in-memory counters don't survive a restart. The event log
// is authoritative because state.json can lag a crash-fsynced run.finished.
func ActiveRunCounts(runsDir string) (map[string]int, error) {
	counts, _, err := scanActiveRuns(runsDir)
	return counts, err
}

func scanActiveRuns(runsDir string) (map[string]int, map[string]string, error) {
	counts := map[string]int{}
	runs := map[string]string{}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return counts, runs, nil
		}
		return nil, nil, err
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
			return nil, nil, fmt.Errorf("read event-log phase for run %q: %w", id.RunID, err)
		}
		if phase == journal.PhaseRunning {
			counts[id.Workflow]++
			runs[id.RunID] = id.Workflow
		}
	}
	return counts, runs, nil
}
