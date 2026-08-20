package localscheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readprobe"
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
	scoped, _, err := activeRuns([]string{runsDir})
	counts := map[string]int{}
	for identity, count := range scoped {
		counts[identity.Workflow] += count
	}
	return counts, err
}

// ActiveRunCountsByWorkflowDirs returns active counts across several gaggle
// run roots.
func ActiveRunCountsByWorkflowDirs(runsDirs []string) (map[WorkflowIdentity]int, error) {
	return ActiveRunCountsByWorkflowDirsContext(context.Background(), runsDirs)
}

// ActiveRunCountsByWorkflowDirsContext returns active counts while checking
// ctx between run roots and journal reads.
func ActiveRunCountsByWorkflowDirsContext(ctx context.Context, runsDirs []string) (map[WorkflowIdentity]int, error) {
	counts, _, err := activeRunsContext(ctx, runsDirs)
	return counts, err
}

func activeRuns(runsDirs []string) (map[WorkflowIdentity]int, map[string]WorkflowIdentity, error) {
	return activeRunsContext(context.Background(), runsDirs)
}

func activeRunsContext(ctx context.Context, runsDirs []string) (map[WorkflowIdentity]int, map[string]WorkflowIdentity, error) {
	counts := map[WorkflowIdentity]int{}
	runs := map[string]WorkflowIdentity{}
	for _, runsDir := range runsDirs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		err := visitActiveRunsContext(ctx, runsDir, func(id journal.RunIdentity) {
			identity := WorkflowIdentity{Gaggle: id.Gaggle, Workflow: id.Workflow}
			counts[identity]++
			runs[id.RunID] = identity
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return counts, runs, nil
}

func visitActiveRunsContext(ctx context.Context, runsDir string, visit func(journal.RunIdentity)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir() {
			continue
		}
		readprobe.RecordActiveScanDir()
		dir := filepath.Join(runsDir, e.Name())
		rd, err := journal.OpenRead(dir)
		if err != nil {
			if errors.Is(err, journal.ErrNotRunDirectory) {
				continue
			}
			return fmt.Errorf("open run journal %q: %w", e.Name(), err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		readprobe.RecordActiveScanOpen()
		// Phase first, identity second. Both are ordered by cost against how
		// many runs reach them: phase comes from a bounded tail read that
		// decides essentially every run from its last record (#2755), while
		// identity is a YAML parse of run.yaml — and on a long-lived instance
		// all but a handful of runs are terminal, so parsing every run.yaml
		// buys nothing. The old order paid both for all 54,333 of them.
		phase, err := rd.PhaseBounded(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// The old order read run.yaml first and skipped any directory whose
			// identity would not parse, so a phase failure was only ever
			// reported for a run this scan could name. Keep that: a directory
			// this reconciliation cannot attribute to a workflow is not one it
			// should refuse to boot over.
			if _, idErr := rd.Identity(); idErr != nil {
				continue
			}
			return fmt.Errorf("read phase for run %q: %w", e.Name(), err)
		}
		if phase != journal.PhaseRunning {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		id, err := rd.Identity()
		if err != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visit(id)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}
