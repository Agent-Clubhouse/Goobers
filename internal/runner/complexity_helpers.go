package runner

import (
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func parallelBranchTimedOut(ws *walkState) bool {
	if ws.parallel == nil || ws.state == workflow.TargetJoin {
		return false
	}
	deadline := ws.parallel.currentDeadline()
	if deadline.IsZero() || time.Now().Before(deadline) {
		return false
	}
	ws.parallel.markCurrentTimedOut()
	ws.state = workflow.TargetJoin
	return true
}

func parallelTerminalOutcome(outcomes []*parallelBranchResult) (string, *parallelTaskTerminal, *parallelGateTerminal) {
	for _, outcome := range outcomes {
		if outcome != nil && outcome.terminalTarget != "" {
			return outcome.terminalTarget, outcome.terminalTask, outcome.terminalGate
		}
	}
	return "", nil, nil
}

func captureParallelBranchBinding(result *parallelBranchResult, initial, current *apiv1.RepoRef, revision **apiv1.WorkspaceRevision) {
	result.workspaceRevision = (*revision).DeepCopy()
	if !reposEqual(initial, current) {
		result.repoRef = current
	}
}

func cancelQueuedParallelBranches(
	jr *journal.Run,
	par *parallelExec,
	p apiv1.Parallel,
	queue []int,
	next *int,
	outcomes []*parallelBranchResult,
	baseCompleted stageOutputs,
	branchEvents parallelBranchEventIndex,
	in StartInput,
) error {
	for *next < len(queue) {
		index := queue[*next]
		branch := par.branchSnapshot(index)
		cursors := par.settleBranch(
			branch.id, journal.BranchCancelled, branch.artifacts, branch.pointers,
			branch.produced, branch.failed, branch.noOutput,
		)
		jr.SetBranchCursors(cursors)
		if err := jr.Append(journal.Event{
			Type:         journal.EventBranchFinished,
			Branch:       branch.id,
			Parallel:     p.Name,
			BranchName:   branch.name,
			BranchStatus: journal.BranchCancelled,
		}); err != nil {
			return err
		}
		outcomes[index] = &parallelBranchResult{
			index:     index,
			status:    journal.BranchCancelled,
			pointers:  branch.pointers,
			completed: branchStageOutputs(baseCompleted, branchEvents.events(branch.id), in.Machine),
			artifacts: branch.artifacts,
			produced:  branch.produced,
			failed:    branch.failed,
			noOutput:  branch.noOutput,
		}
		*next = *next + 1
	}
	return nil
}

func cancelQueuedWhenTriggered(triggered bool, jr *journal.Run, par *parallelExec, p apiv1.Parallel, queue []int, next *int, outcomes []*parallelBranchResult, baseCompleted stageOutputs, branchEvents parallelBranchEventIndex, in StartInput) error {
	if !triggered {
		return nil
	}
	return cancelQueuedParallelBranches(jr, par, p, queue, next, outcomes, baseCompleted, branchEvents, in)
}

func (r *Runner) acceptTaskWorkspaceRevision(
	jr executionJournal,
	task string,
	attempt int,
	class journal.AttemptClass,
	in *StartInput,
	current **apiv1.WorkspaceRevision,
	repoRef *apiv1.RepoRef,
	revision *apiv1.WorkspaceRevision,
) error {
	configuredRepo, err := workspacerevision.Resolve(*revision, in.RepoRef, r.cfg.AdditionalRepos)
	if err != nil {
		errorCode := workspacerevision.CodeUnauthorized
		var revisionErr *workspacerevision.Error
		if errors.As(err, &revisionErr) {
			errorCode = revisionErr.Code
		}

		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: task, Attempt: attempt, AttemptClass: class,
			Error: &journal.ErrorDetail{Code: errorCode, Message: err.Error()},
		}); aerr != nil {
			return fmt.Errorf("runner: journal workspace revision rejection for %q: %w", task, aerr)
		}
		return fmt.Errorf("runner: stage %q workspace revision rejected: %w", task, err)
	}
	accepted, err := workspacerevision.Accept(*current, revision, true, true)
	if err != nil {
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: task, Attempt: attempt, AttemptClass: class,
			Error: &journal.ErrorDetail{Code: workspacerevision.CodeConflict, Message: err.Error()},
		}); aerr != nil {
			return fmt.Errorf("runner: journal workspace revision rejection for %q: %w", task, aerr)
		}
		return fmt.Errorf("runner: stage %q workspace revision rejected: %w", task, err)
	}
	in.RepoRef = configuredRepo
	if repoRef != nil {
		*repoRef = configuredRepo
	}
	*current = accepted.DeepCopy()
	in.workspaceRevision = accepted.DeepCopy()
	return nil
}

func (r *Runner) prepareTaskResult(
	jr executionJournal,
	task apiv1.Task,
	result *apiv1.ResultEnvelope,
	upstream []apiv1.ContextPointer,
	in *StartInput,
	tf taskFrame,
	attempt int,
	class journal.AttemptClass,
) error {
	result.Artifacts = normalizeArtifactIntegrity(task.Type, result.Artifacts)
	*result = r.validateDependencyResult(jr, task.Name, *result, upstream)
	if task.Type != apiv1.TaskDeterministic && result.WorkspaceRevision != nil {
		result.WorkspaceRevision = nil
		result.Status = apiv1.ResultFailure
		result.Error = &apiv1.ErrorInfo{
			Code:    workspacerevision.CodeUnauthorized,
			Message: "agentic results cannot establish workspace revision authority",
		}
		result.Summary = "workspace revision authority is restricted to deterministic stages"
	}
	if task.Type == apiv1.TaskDeterministic &&
		result.Status == apiv1.ResultSuccess &&
		result.WorkspaceRevision != nil {
		current := &in.workspaceRevision
		if tf.workspaceRevision != nil {
			current = tf.workspaceRevision
		}
		if err := r.acceptTaskWorkspaceRevision(jr, task.Name, attempt, class, in,
			current, tf.repoRef, result.WorkspaceRevision); err != nil {
			return err
		}
	}
	return nil
}

func applyTaskUsageBudget(
	limits apiv1.Limits,
	usage *attemptUsageCollector,
	totals *stageUsageTotals,
	result *apiv1.ResultEnvelope,
	dispatchErr *error,
) {
	attemptUsage, usageReported := usage.snapshot()
	accumulateStageUsage(totals, attemptUsage)
	if *dispatchErr == nil || usageReported {
		var budgetExceeded bool
		*result, budgetExceeded = enforceStageBudget(limits, attemptUsage, totals, *result)
		if budgetExceeded {
			*dispatchErr = nil
		}
	}
}

func (r *Runner) settledParallelBranchResult(
	branch branchState,
	history []journal.Event,
	baseCompleted stageOutputs,
	in StartInput,
	index int,
) (*parallelBranchResult, string, *parallelTaskTerminal, *parallelGateTerminal, error) {
	lastStage, lastResult, _ := lastFinishedSubject(history)
	terminalTarget, terminalTask, terminalGate := parallelBranchTerminal(history, in.Machine)
	workspaceRevision, err := reconstructWorkspaceRevision(history)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("runner: reconstruct settled parallel branch %d workspace revision: %w", branch.id, err)
	}
	var repoRef *apiv1.RepoRef
	if workspaceRevision != nil {
		resolvedRepo, err := workspacerevision.Resolve(*workspaceRevision, in.RepoRef, r.cfg.AdditionalRepos)
		if err != nil {
			return nil, "", nil, nil, fmt.Errorf("runner: resolve settled parallel branch %d workspace revision repository: %w", branch.id, err)
		}
		if !reposEqual(&resolvedRepo, &in.RepoRef) {
			repoRef = &resolvedRepo
		}
	}
	return &parallelBranchResult{
		index:             index,
		status:            branch.status,
		lastStage:         lastStage,
		lastResult:        lastResult,
		pointers:          branch.pointers,
		completed:         branchStageOutputs(baseCompleted, history, in.Machine),
		artifacts:         branch.artifacts,
		produced:          branch.produced,
		failed:            branch.failed,
		noOutput:          branch.noOutput,
		workspaceRevision: workspaceRevision,
		repoRef:           repoRef,
		terminalTarget:    terminalTarget,
		terminalTask:      terminalTask,
		terminalGate:      terminalGate,
	}, terminalTarget, terminalTask, terminalGate, nil
}

func initialParallelBranchResult(
	branch branchState,
	baseLastStage string,
	baseLastResult apiv1.ResultEnvelope,
	baseCompleted stageOutputs,
	in StartInput,
	history []journal.Event,
) parallelBranchResult {
	return parallelBranchResult{
		index:      branch.id - 1,
		lastStage:  baseLastStage,
		lastResult: baseLastResult,
		completed:  branchStageOutputs(baseCompleted, history, in.Machine),
		pointers:   append([]apiv1.ContextPointer(nil), branch.pointers...),
		artifacts:  branch.artifacts,
		produced:   branch.produced,
		failed:     branch.failed,
		noOutput:   branch.noOutput,
	}
}
