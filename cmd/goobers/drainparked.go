package main

import (
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// parkedRun is a run that exists, is not terminal, and is not held by the
// drain — i.e. one the daemon will exit without waiting for.
type parkedRun struct {
	RunID    string
	Workflow string
}

// parkedNonTerminalRuns reports runs that are non-terminal but absent from the
// drain's own view of in-flight work (#3453).
//
// The drain waits on a WaitGroup held across trackedStarter.Start, and Start
// RETURNS when a run pauses at a gate — not only when it terminates. So a
// gate-paused run holds no WaitGroup entry and no registry entry: wg.Wait()
// does not wait for it, and ActiveRuns() cannot see it. The drain then prints
// "no in-flight runs remain" while a non-terminal run is sitting there.
//
// That message is the defect this addresses. Whether shutdown SHOULD block on
// a parked run is a separate question, and the answer since #3426 is no: the
// boot sweep hands Resume a nil machine on digest drift and Resume
// reconstructs the historical definition via PinnedWorkflowMachine, so a
// parked run is recovered automatically at next boot rather than orphaned.
// Blocking shutdown on it would trade a solved problem for a stuck drain.
//
// What was not solved is the operator being TOLD it is safe to restart when a
// non-terminal run exists. "Safe to restart" should be an answer the system
// gives, not one inferred from a message that is silent about the population
// it cannot see.
//
// Runs listed in active are excluded: those are genuinely held and already
// reported. Errors are swallowed per run — this is a reporting path on a
// shutdown path, and a run whose journal cannot be read must not be able to
// stall or fail the drain.
func parkedNonTerminalRuns(l instance.Layout, active []trackedRun) []parkedRun {
	held := make(map[string]struct{}, len(active))
	for _, run := range active {
		held[run.RunID] = struct{}{}
	}
	runDirs, err := l.RunDirs()
	if err != nil {
		return nil
	}
	var parked []parkedRun
	seen := make(map[string]struct{})
	for _, runsDir := range runDirs {
		entries, exists, err := readDirectory(runsDir)
		if err != nil || !exists {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			rd, err := journal.OpenRead(filepath.Join(runsDir, e.Name()))
			if err != nil {
				continue
			}
			id, err := rd.Identity()
			if err != nil {
				continue
			}
			if _, ok := held[id.RunID]; ok {
				continue
			}
			if _, dup := seen[id.RunID]; dup {
				continue
			}
			// Event-log-first (#242), same rule the boot sweep uses:
			// state.json can lag a crash-fsynced run.finished event, so the
			// phase reconstructed from the log decides terminality. Reading
			// the checkpoint instead would report freshly-finished runs as
			// parked.
			phase, err := rd.Phase()
			if err != nil {
				continue
			}
			switch phase {
			case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
				continue
			}
			seen[id.RunID] = struct{}{}
			parked = append(parked, parkedRun{RunID: id.RunID, Workflow: id.Workflow})
		}
	}
	sort.Slice(parked, func(i, j int) bool {
		if parked[i].Workflow != parked[j].Workflow {
			return parked[i].Workflow < parked[j].Workflow
		}
		return parked[i].RunID < parked[j].RunID
	})
	return parked
}
