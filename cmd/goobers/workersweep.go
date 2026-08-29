package main

// workersweep.go is decision 003's worker-hygiene graft: goobers-worker — and
// ONLY goobers-worker — sweeps the stage pods it left behind, resolving each
// one's fate from the engine.
//
// Two constraints shape everything here, and both come from the record.
//
// The daemon never sweeps. It is not the pod creator (decision 011: its
// ServiceAccount keeps automountServiceAccountToken:false and zero grants, and
// allow-daemon-egress does not reach the apiserver), so it has no list/delete
// verb to sweep with and must not acquire one. The worker already creates
// these pods; reclaiming them is its own housekeeping.
//
// The sweep fails closed toward LEAVING a pod. dispatcher.SweepOrphans deletes
// only what a resolver POSITIVELY settles, and this resolver settles an
// attempt only on an answer from Temporal — Completed, Failed, or no such
// execution. An unreachable frontend, a timed-out describe, an ambiguous
// answer: all leave the pod, and the pod's always-on activeDeadlineSeconds
// stamp reclaims it instead. That direction is the whole point. Decision 003
// rejected the in-process-dispatcher option partly because the pre-existing
// sweep was fail-closed toward DELETION and "would delete the worker's
// engine-start pods"; deleting a pod whose stage is still running destroys
// in-flight work, and for a mutating stage (open-pr, push-branch, merge-pr)
// does it where nobody sees.

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
)

const (
	// workerSweepBudget bounds the WHOLE sweep. It runs before the worker
	// starts polling, so an engine that answers slowly must cost the worker a
	// bounded startup delay and nothing more — an unswept pod is reclaimed by
	// its deadline stamp, an unstarted worker serves no queue at all.
	workerSweepBudget = 90 * time.Second
	// workerSweepDescribeTimeout bounds ONE describe within that budget.
	workerSweepDescribeTimeout = 10 * time.Second
)

// workerSweepDescriber is the one Temporal call the sweep needs.
// client.Client satisfies it; tests substitute a fake, so no server is needed
// to prove the adopt/dispose/leave decisions.
type workerSweepDescriber interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// dialWorkerSweepTemporal is a seam so tests drive the sweep without a server.
var dialWorkerSweepTemporal = bootstrap.DialTemporal

// stageOrphanSweeper is the sweep half of *dispatcher.Dispatcher, named as an
// interface so the wiring can be exercised with a fake.
type stageOrphanSweeper interface {
	SweepOrphans(ctx context.Context, runs dispatcher.RunStates) ([]string, error)
}

// temporalRunStates answers dispatcher.SweepOrphans from the engine.
type temporalRunStates struct {
	client workerSweepDescriber
}

// sweepStatus is what ONE describe established about one workflow id.
type sweepStatus int

const (
	// sweepUnresolved: the describe did not answer (transport, timeout,
	// anything that is not the workflow telling us it does not exist).
	sweepUnresolved sweepStatus = iota
	// sweepRunning: the execution exists and is still running.
	sweepRunning
	// sweepSettled: the execution exists and has closed, or there is no such
	// execution at all. Both mean nothing is driving this attempt.
	sweepSettled
)

// RunState resolves one labeled stage pod's attempt.
//
// TWO workflow identities can legitimately own a stage pod, because decision
// 003 leaves two drivers in the tree:
//
//   - <runID>/<stage>/<attempt>, the per-attempt engine.DispatchOne workflow a
//     daemon-driven (runner-driven) run starts for a placed stage;
//   - <runID> itself, the engine's Run workflow, for an engine-start run whose
//     walk dispatches the stage from inside the run.
//
// The pod's labels do not say which, so both are asked. Live wins outright:
// if EITHER is still executing, something may still be writing through this
// pod and it is adopted. Disposal needs every id settled; anything
// unresolved leaves the pod.
func (r temporalRunStates) RunState(ctx context.Context, attempt dispatcher.PodAttempt) dispatcher.RunState {
	ids := []string{
		engine.DispatchOneWorkflowID(attempt.RunID, attempt.Stage, int32(attempt.Attempt)),
		attempt.RunID,
	}
	settled := 0
	for _, id := range ids {
		switch r.status(ctx, id) {
		case sweepRunning:
			return dispatcher.RunStateLive
		case sweepSettled:
			settled++
		}
	}
	if settled == len(ids) {
		return dispatcher.RunStateTerminal
	}
	return dispatcher.RunStateIndeterminate
}

// status runs one bounded describe. It deliberately does NOT retry: the sweep
// is hygiene running ahead of the worker's first poll, and the cost of giving
// up is that a settled pod lives until its deadline — whereas the cost of
// waiting is a worker that is not serving any queue.
func (r temporalRunStates) status(ctx context.Context, workflowID string) sweepStatus {
	if r.client == nil {
		return sweepUnresolved
	}
	describeCtx, cancel := context.WithTimeout(ctx, workerSweepDescribeTimeout)
	defer cancel()
	desc, err := r.client.DescribeWorkflowExecution(describeCtx, workflowID, "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			// No execution under this id. That is an ANSWER, not a failure:
			// nothing on the engine is addressable as this attempt.
			return sweepSettled
		}
		return sweepUnresolved
	}
	if desc.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return sweepRunning
	}
	return sweepSettled
}

// sweepWorkerStageOrphans runs one bounded sweep at worker boot, before the
// worker starts polling.
//
// It is never fatal. A worker that cannot sweep is still a worker that can
// serve every queue it was started for, and refusing to start over a hygiene
// failure would turn an unreachable frontend into an outage. Failures are
// reported on stderr and the worker proceeds.
//
// The Temporal client is dialed and closed here rather than shared with
// workerhost: the host dials inside Run, which has not been called yet, and a
// boot-time connection that lives for the length of one sweep costs nothing to
// reason about.
func sweepWorkerStageOrphans(sweeper stageOrphanSweeper, hostPort, namespace string, stdout, stderr io.Writer) {
	if sweeper == nil {
		return
	}
	c, err := dialWorkerSweepTemporal(hostPort, namespace)
	if err != nil {
		pf(stderr, "goobers worker: orphan sweep skipped: dial temporal %s (namespace %s): %v\n", hostPort, namespace, err)
		return
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), workerSweepBudget)
	defer cancel()
	disposed, err := sweeper.SweepOrphans(ctx, temporalRunStates{client: c})
	if err != nil {
		pf(stderr, "goobers worker: orphan sweep: %v\n", err)
	}
	if len(disposed) == 0 {
		pf(stdout, "goobers worker: orphan sweep: nothing settled to dispose\n")
		return
	}
	pf(stdout, "goobers worker: orphan sweep disposed %d settled stage pod(s): %s\n",
		len(disposed), strings.Join(disposed, ", "))
}
