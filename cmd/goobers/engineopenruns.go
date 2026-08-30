package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// engineOpenRunScanTimeout bounds the one boot-time open-workflow scan. It is
// generous — a namespace with thousands of open workflows pages — but bounded,
// because a wedged Temporal frontend must delay daemon startup, not prevent
// it: a failed scan degrades to the pre-#3876 behaviour (direct runs still
// reattach by run id), it does not fail the boot.
const engineOpenRunScanTimeout = 60 * time.Second

// attachEngineOpenRunResolver performs the daemon's single boot-time
// open-workflow scan and returns guards that can name the Temporal workflow
// behind any run id, plus the runs the scan found.
//
// # Why this exists (decision 005 D1, piece 6)
//
// The daemon's restart story for an engine-driven run is: find runs/<id>/,
// see `driver: engine`, and reattach by describing the workflow. Two holes in
// that story become load-bearing the moment real triggers can start engine
// runs:
//
//  1. A SCHEDULED engine run's workflow id is not its run id (RunScheduled
//     hashes the claim id), so the describe returns NotFound. NotFound is
//     treated as settled, and settlement releases the scheduler's reconciled
//     concurrency slot — underneath a workflow that is still executing.
//     Worse, the slot release invites the scheduler to admit a SECOND run of
//     the same workflow, which is the duplicate-driver hazard the guards were
//     written to prevent, reached from the recovery path.
//
//  2. The reservation this daemon writes before calling Temporal
//     (engine.ReserveRun) closes the start-to-first-emit window from the disk
//     side. This scan closes it from the engine side: a workflow that is open
//     for one of our gaggles and has no local run directory at all is
//     reported, so an operator sees it instead of it silently continuing to
//     run with nothing in this process watching.
//
// The gaggle filter is what keeps sibling instances sharing one Temporal
// namespace from reattaching to each other's runs, and it is fail-closed: an
// empty owned-gaggle set matches nothing.
func attachEngineOpenRunResolver(ctx context.Context, client *daemonEngineClient, guards *engineRunGuards, gaggles map[string]struct{}) (*engineRunGuards, map[string]engine.OpenRun, error) {
	if client == nil || client.Temporal() == nil || guards == nil {
		return guards, nil, nil
	}
	scanCtx, cancel := context.WithTimeout(ctx, engineOpenRunScanTimeout)
	defer cancel()
	open, err := engine.NewWorkflowLiveness(client.Temporal(), client.Namespace()).OpenRuns(scanCtx, gaggles)
	if err != nil {
		return guards, nil, fmt.Errorf("scan open engine runs: %w", err)
	}
	return guards.withWorkflowIDResolver(func(runID string) (string, bool) {
		run, ok := open[runID]
		return run.WorkflowID, ok
	}), open, nil
}

// ownedGaggleSet is the set of gaggle names this daemon serves, in the shape
// OpenRuns filters by.
func ownedGaggleSet[T any](machines map[localscheduler.WorkflowIdentity]T) map[string]struct{} {
	out := make(map[string]struct{})
	for identity := range machines {
		if identity.Gaggle != "" {
			out[identity.Gaggle] = struct{}{}
		}
	}
	return out
}

// reportOrphanedEngineRuns annotates the instance log for every open engine
// workflow this daemon owns that has no local run directory.
//
// This is the far-side half of the restart window (piece 6). engine.ReserveRun
// makes the ordering reservation-then-start, so under normal operation the
// disk record always exists first and the resume scan finds it. An orphan
// here therefore means something genuinely wrong — a lost or hand-deleted run
// directory, or a workflow started by a daemon whose disk state did not
// survive — and the daemon's only correct move is to make it VISIBLE. It
// deliberately does not cancel: a workflow this process cannot account for is
// not one it should be terminating on a guess.
//
// "Accounted for" is the mere EXISTENCE of runs/<id>/, not a readable
// journal. This runs on the boot path against directories other components
// are about to open, and journal.OpenRead migrates the schema it reads — a
// diagnostic scan must not write, and a run mid-reservation whose journal is
// not yet readable is still a run this daemon knows about.
func reportOrphanedEngineRuns(l instance.Layout, log *journal.InstanceLog, open map[string]engine.OpenRun) []string {
	if log == nil || len(open) == 0 {
		return nil
	}
	var orphans []string
	seen := make(map[string]struct{}, len(open))
	for runID, run := range open {
		if _, dup := seen[run.WorkflowID]; dup {
			continue
		}
		gaggleLayout := l
		if run.Gaggle != "" {
			gaggleLayout = l.ForGaggle(run.Gaggle)
		}
		if _, err := os.Stat(filepath.Join(gaggleLayout.RunsDir(), runID)); err == nil {
			continue
		}
		seen[run.WorkflowID] = struct{}{}
		orphans = append(orphans, runID)
		_ = log.Append(journal.Event{
			Type: journal.EventError, Gaggle: run.Gaggle, Workflow: run.Workflow, RunID: runID,
			Reason: "engine workflow open with no local run directory",
			Runner: map[string]any{
				"action":     journal.RecoveryActionUnresolved,
				"driver":     string(journal.DriverEngine),
				"workflowId": run.WorkflowID,
			},
			Error: &journal.ErrorDetail{
				Code: "engine_run_orphaned",
				Message: fmt.Sprintf(
					"engine workflow %s is open for run %s but this instance has no runs/%s directory; the run is executing with no local record",
					run.WorkflowID, runID, runID),
			},
		})
	}
	return orphans
}
