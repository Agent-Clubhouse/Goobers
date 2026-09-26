package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// enginerunoperator.go routes `goobers run cancel` and `goobers run abort` on
// an ENGINE-DRIVEN run to the engine (decision 005 D2, #3877; decision 003
// ruling 5, carried forward).
//
// Before this, both commands refused. The refusal was right about the hazard —
// `run abort` appends a terminal event straight into a run's own journal, and
// on an engine-driven run that forges a terminal for a workflow that keeps
// executing and keeps emitting into the same file — but it left the operator
// with no way to stop the run at all: the message pointed at "cancel that
// workflow", which meant leaving Goobers for the Temporal CLI, deriving a
// workflow id the operator cannot derive for a scheduled run, and doing it
// with no record in the instance log.
//
// The correct action was always available and is what the stalled-run sweep
// already does: CancelWorkflow. The engine then writes the run's terminal
// event ITSELF — internal/engine's cancel arm records run_failed +
// run.finished(aborted) through a disconnected context — so the run closes out
// on the same journal plane that has been authoring it all along, and nothing
// in this process ever writes to that journal.
//
// # What this deliberately does not do
//
//   - It does not append to the run's journal. Not a terminal, not an
//     annotation. The engine's workflow is that file's only writer.
//   - It does not wait for the run to reach a terminal phase. A cancellation
//     is a REQUEST; the engine lands it, and `goobers run status` (or the
//     daemon's own re-attachment) reports the outcome.
//   - It does not fall back to terminalizing the journal when the workflow
//     cannot be reached or named. The fallback IS the corruption. An
//     unresolvable run is reported and the command fails.

// engineRunCancelOutcome is what one operator-initiated cancellation learned.
type engineRunCancelOutcome struct {
	// WorkflowID is the Temporal workflow the cancellation was addressed to.
	// For a scheduled run it is NOT the run id, and it is the only handle an
	// operator has to go confirm the cancellation in Temporal — which is
	// precisely the id they could not have derived themselves.
	WorkflowID string
}

// cancelEngineDrivenRun asks the engine to cancel an engine-driven run's
// workflow, from a short-lived CLI process.
//
// It dials Temporal itself rather than delegating to the daemon's
// pending-cancels protocol. `run abort` is the daemon-DOWN repair path and
// must keep working with no daemon at all; and an engine run is not one the
// daemon is executing in-process, so the daemon's cancel sweep — which
// resolves an owning Runner — has nothing to resolve for it either. Both
// commands therefore take the same path, which is also the only way the two
// can be guaranteed to behave identically.
func cancelEngineDrivenRun(ctx context.Context, l instance.Layout, identity journal.RunIdentity) (engineRunCancelOutcome, error) {
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return engineRunCancelOutcome{}, fmt.Errorf("load instance config to reach the engine: %w", err)
	}
	engineClient, err := newDaemonEngineClient(cfg)
	if err != nil {
		return engineRunCancelOutcome{}, err
	}
	if engineClient == nil {
		// An engine-driven journal on an instance that cannot reach the
		// engine is a misconfiguration, and the pre-guard behaviour (forge a
		// terminal) is exactly the corruption the guards exist to prevent.
		return engineRunCancelOutcome{}, fmt.Errorf("cancel engine run %s: %w", identity.RunID, errNoEngineClient)
	}
	defer engineClient.Close()

	// Gaggle containment, from the run's own identity: the open-workflow
	// inverse a NotFound cancel resolves through is namespace-wide, and the
	// only gaggle this command has any business resolving through is the one
	// the run it was given belongs to.
	guards, _, scanErr := attachEngineOpenRunResolver(
		ctx, engineClient, engineClient.Guards(), map[string]struct{}{identity.Gaggle: {}})
	if scanErr != nil {
		// The resolver is installed regardless (a boot scan is not the
		// resolver), so a failed enumeration here is not fatal: the cancel is
		// attempted against the run's own id first, which is the right
		// address for every direct run, and the resolver retries the scan if
		// that comes back NotFound.
		_ = scanErr
	}
	workflowID, err := guards.cancelResolved(ctx, identity.RunID)
	if err != nil {
		if errors.Is(err, errEngineRunUnresolvable) {
			return engineRunCancelOutcome{}, fmt.Errorf(
				"%w; the run's journal is NOT terminalized, because forging a terminal is the corruption this refuses to commit",
				err)
		}
		return engineRunCancelOutcome{}, err
	}
	return engineRunCancelOutcome{WorkflowID: workflowID}, nil
}

// runEngineDrivenCancel is the shared CLI arm for `run cancel` and
// `run abort` on an engine-driven run: cancel, record, report. It returns the
// process exit code — 0 for a cancellation the engine accepted, 1 for a
// business refusal (no engine configured, or a run the engine cannot name).
func runEngineDrivenCancel(l instance.Layout, identity journal.RunIdentity, action string, stdout, stderr io.Writer) int {
	outcome, err := cancelEngineDrivenRun(context.Background(), l, identity)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// The instance log, not the run's journal: an operator action on a run
	// this process does not drive belongs in the same recovery record the
	// stalled-run sweep's cancellations are written to, under the same action
	// vocabulary a log scraper already greps for.
	if log, _, logErr := journal.OpenInstanceLog(l.SchedulerDir()); logErr == nil {
		appendErr := log.Append(journal.Event{
			Type: journal.EventRunnerAnnotation, Gaggle: identity.Gaggle,
			Workflow: identity.Workflow, RunID: identity.RunID,
			Runner: map[string]any{
				"kind":       journal.RunnerAnnotationRunRecovery,
				"reason":     action + " requested by an operator",
				"action":     journal.RecoveryActionEngineCancelRequested,
				"driver":     string(journal.DriverEngine),
				"workflowId": outcome.WorkflowID,
			},
		})
		if appendErr != nil {
			pf(stderr, "warning: record engine cancellation for run %s: %v\n", identity.RunID, appendErr)
		}
		_ = log.Close()
	} else {
		pf(stderr, "warning: open instance log to record engine cancellation for run %s: %v\n", identity.RunID, logErr)
	}
	pf(stdout, "requested cancellation of engine-driven run %s (Temporal workflow %s); "+
		"the engine writes the run's terminal event once the cancellation lands\n",
		identity.RunID, outcome.WorkflowID)
	return 0
}
