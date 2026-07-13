package localscheduler

import (
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
)

// ActiveRunCounts scans runsDir for non-terminal runs and returns per-workflow
// active counts — the daemon-startup reconciliation Conditions.Reconcile needs,
// since Conditions' in-memory counters don't survive a restart. A run whose
// state.json can't be read (e.g. mid-recovery) is conservatively counted as
// active rather than dropped, so a restart never silently exceeds max-parallel.
func ActiveRunCounts(runsDir string) (map[string]int, error) {
	counts := map[string]int{}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return counts, nil
		}
		return nil, err
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
		st, err := rd.State()
		if err != nil {
			counts[id.Workflow]++
			continue
		}
		if st.Phase == journal.PhaseRunning {
			counts[id.Workflow]++
		}
	}
	return counts, nil
}
