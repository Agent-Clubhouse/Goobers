package runner

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Branch time budgets are checked between stages, never by killing an active
// invocation. Durable child waiting consumes neither its branch execution
// allowance nor a step. Other runnable branches retain their own clocks.
func (r *Runner) stopParallelBranchAtBoundary(ctx context.Context, run *journal.Run, in StartInput, par *parallelExec, branch branchState, steps *atomic.Int64, result *parallelBranchResult, restored parallelStageRecovery) bool {
	if ctx.Err() != nil {
		result.status, result.paused = journal.BranchCancelled, parallelDrainCancellation(ctx)
		return true
	}
	// Reentering the same stage to restore child custody is not the next stage
	// boundary. It must finish restoring/settling its child before a branch
	// deadline or a new step allowance can terminate subsequent execution.
	if restored.child != nil || restored.parent {
		return false
	}
	expired, err := parallelBranchExpired(run, in, par.spec.Name, branch, par.spec.BranchTimeoutSeconds, time.Now())
	if err != nil {
		result.status, result.err = journal.BranchFailed, err
		return true
	}
	if expired {
		result.status, result.failed = journal.BranchTimedOut, true
		return true
	}
	if steps.Add(1) > int64(r.maxSteps) {
		result.status = journal.BranchFailed
		result.err = fmt.Errorf("runner: run %q exceeded max steps (%d): possible loop", in.RunID, r.maxSteps)
		return true
	}
	return false
}

func parallelBranchExpired(run *journal.Run, in StartInput, parallel string, branch branchState, seconds int32, now time.Time) (bool, error) {
	deadline := branch.deadline(seconds)
	if deadline.IsZero() {
		return false, nil
	}
	if !hasContainedParentStage(in) {
		return !now.Before(deadline), nil
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return false, err
	}
	events, err := reader.Events()
	if err != nil {
		return false, err
	}
	// Branch IDs are reused in later parallel visits. Only the active block's
	// history may grant waiting time back to this branch.
	history := newParallelBranchEventIndex(events, parallel).events(branch.id)
	elapsed, err := journal.ChildExecutionElapsed(history, branch.startedAt, now, &branch.id)
	if err != nil {
		return false, err
	}
	return elapsed >= time.Duration(seconds)*time.Second, nil
}
