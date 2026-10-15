package runner

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// concurrentParallelRunner runs one parallel visit through the bounded
// dispatcher; walk supplies it as a closure over its own state.
type concurrentParallelRunner func(visit parallelVisit) (Result, bool, error)

// enterParallel fans the walk out into parallel p. A selective parallel first
// freezes its selection; a source that cannot be frozen fails the parallel
// state along its onFailure route instead of fanning out. The dispatcher runs
// the whole visit; the serial path starts the first runnable branch and lets
// walk advance it. done reports that walk must return result.
func (r *Runner) enterParallel(ctx context.Context, ws *walkState, p apiv1.Parallel, runConcurrent concurrentParallelRunner) (Result, bool, error) {
	if err := supportedFailurePolicy(p.FailurePolicy); err != nil {
		res, failErr := r.failTerminal(ctx, ws.in.RunID, ws.jr, ws.in.RepoRef, ws.state, ws.steps,
			fmt.Errorf("runner: parallel %q: %w", p.Name, err))
		return res, true, failErr
	}
	selection, err := r.freezeParallelSelection(ctx, ws.jr, ws.in.Machine, p)
	if err != nil {
		next, res, done, routeErr := r.routeFanOutSourceFailure(ws, p, err)
		if done || routeErr != nil {
			return res, true, routeErr
		}
		ws.state = next
		return Result{}, false, nil
	}
	if parallelUsesDispatcher(ws.in.Machine, p) {
		return runConcurrent(parallelVisit{spec: p, selection: selection})
	}
	ws.parallel = newParallelExec(p)
	ws.parallel.applySelection(selection)
	ws.fanIn = nil
	ws.parallelRootPointers = append([]apiv1.ContextPointer(nil), ws.pointers...)
	ws.jr.SetMachineState(ws.state)
	if err := ws.jr.Append(journal.Event{
		Type: journal.EventParallelStarted, Parallel: p.Name,
		Completeness: ws.parallel.completeness(), Selection: selection,
	}); err != nil {
		res, failErr := r.failTerminal(ctx, ws.in.RunID, ws.jr, ws.in.RepoRef, ws.state, ws.steps, err)
		return res, true, failErr
	}
	first := ws.parallel.current()
	ws.jr.SetBranchCursors(ws.parallel.cursors())
	if first == nil {
		// Every branch was skipped: the join still runs exactly once.
		ws.state = workflow.TargetJoin
		return Result{}, false, nil
	}
	if err := ws.jr.Append(journal.Event{
		Type: journal.EventBranchStarted, Branch: first.id,
		Parallel: p.Name, BranchName: first.name, Stage: first.start,
	}); err != nil {
		res, failErr := r.failTerminal(ctx, ws.in.RunID, ws.jr, ws.in.RepoRef, ws.state, ws.steps, err)
		return res, true, failErr
	}
	first.started = true
	first.startedAt = time.Now()
	ws.jr.SetBranch(first.id)
	ws.state = first.start
	return Result{}, false, nil
}
