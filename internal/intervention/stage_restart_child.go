package intervention

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
)

// ChildStageRestartAdmission is constructed by the trusted generated-runtime
// adapter after durable epoch acceptance. Entry is its isolated pinned machine;
// Fence must invoke the callback under WithChildExecutionResume for this exact
// queued epoch. The callback must not call the queue again.
type ChildStageRestartAdmission struct {
	Entry localscheduler.WorkflowEntry
	Fence func(context.Context, func() error) error
}

func stageRestartReservation(ctx context.Context, plan runner.StageRestartPlan, candidate Execution) (func(*localscheduler.Scheduler) (func(), error), error) {
	child, admitted := plan.Source.Child, candidate.ChildRestart
	if child == nil {
		if admitted != nil || plan.Continuation.ChildContinuation != nil {
			return nil, interventionConflict("restart_custody_changed", "Ordinary restart cannot acquire child admission.")
		}
		return nil, nil
	}
	if admitted == nil || admitted.Fence == nil || plan.Continuation.ChildContinuation == nil || admitted.Entry.Gaggle != plan.Source.Gaggle || admitted.Entry.Workflow != plan.Source.Workflow {
		return nil, interventionConflict("restart_child_admission_unavailable", "Generated restart requires exact queued child admission.")
	}
	return func(scheduler *localscheduler.Scheduler) (func(), error) {
		return scheduler.ReserveChild(ctx, localscheduler.ChildAdmissionRequest{RunID: plan.Continuation.RunID, ParentRunID: child.ParentRunID, Parent: localscheduler.WorkflowIdentity{Gaggle: plan.Source.Gaggle, Workflow: child.ParentWorkflow}, Child: admitted.Entry}, time.Now())
	}, nil
}

func (a *ChildStageRestartAdmission) fence(ctx context.Context, callback func() error) error {
	if a == nil {
		return callback()
	}
	return a.Fence(ctx, callback)
}

func restartResumeInput(ctx context.Context, lease *interventionExecutionLease, candidate Execution) runner.ResumeInput {
	in := runner.ResumeInput{RunID: lease.resolved.runID, Machine: candidate.Machine, GooberDigest: candidate.GooberDigest, RepoRef: candidate.RepoRef}
	if candidate.ChildRestart != nil {
		// Runner has registered cancellation ownership and owns the journal
		// before this transaction. A later parent cancel can therefore find and
		// stop it; a prior cancel prevents any resumed stage effect.
		in.OnRecoveryOwned = func() error { return candidate.ChildRestart.fence(ctx, func() error { return nil }) }
	}
	return in
}
