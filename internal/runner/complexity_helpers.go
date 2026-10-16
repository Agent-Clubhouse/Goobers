package runner

import (
	"context"
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

func (r *Runner) recordParallelRunBranch(ws *walkState) error {
	if ws.branchRecorded || !machineUsesRepo(ws.in.Machine) {
		return nil
	}
	if err := r.recordRunBranch(ws.jr, ws.in); err != nil {
		return fmt.Errorf("runner: journal run branch for %q: %w", ws.in.RunID, err)
	}
	ws.branchRecorded = true
	return nil
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
	par *parallelExec,
	queue []int,
	next *int,
	outcomes []*parallelBranchResult,
	baseCompleted stageOutputs,
	branchEvents parallelBranchEventIndex,
	in StartInput,
	settle func(parallelBranchResult) error,
) error {
	for *next < len(queue) {
		index := queue[*next]
		branch := par.branchSnapshot(index)
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
		if err := settle(*outcomes[index]); err != nil {
			return err
		}
		*next = *next + 1
	}
	return nil
}

func (r *Runner) acceptTaskWorkspaceRevision(
	ctx context.Context,
	jr executionJournal,
	task string,
	attempt int,
	class journal.AttemptClass,
	in *StartInput,
	current **apiv1.WorkspaceRevision,
	repoRef *apiv1.RepoRef,
	revision *apiv1.WorkspaceRevision,
) error {
	configuredRepo, err := r.resolveWorkspaceRevision(ctx, *revision, in.configuredRepository())
	if err != nil {
		errorCode := workspacerevision.CodeUnauthorized
		var revisionErr *workspacerevision.Error
		if errors.As(err, &revisionErr) {
			errorCode = revisionErr.Code
		}

		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: task, Attempt: attempt, AttemptClass: class,
			Error: journal.ErrorDetailFor(errorCode, err),
		}); aerr != nil {
			return fmt.Errorf("runner: journal workspace revision rejection for %q: %w", task, aerr)
		}
		return fmt.Errorf("runner: stage %q workspace revision rejected: %w", task, err)
	}
	accepted, err := workspacerevision.Accept(*current, revision)
	if err != nil {
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: task, Attempt: attempt, AttemptClass: class,
			Error: journal.ErrorDetailFor(workspacerevision.CodeConflict, err),
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
	ctx context.Context,
	jr executionJournal,
	task apiv1.Task,
	result *apiv1.ResultEnvelope,
	upstream []apiv1.ContextPointer,
	in *StartInput,
	tf taskFrame,
	attempt int,
	class journal.AttemptClass,
) error {
	if err := validateChildWorkspaceResult(*in, *result); err != nil {
		return err
	}
	if err := workspacerevision.NormalizeResult(result, task.Type == apiv1.TaskDeterministic); err != nil {
		return recordWorkspaceRevisionRejection(jr, task.Name, attempt, class, err)
	}
	result.Artifacts = normalizeArtifactIntegrity(task.Type, result.Artifacts)
	*result = r.validateDependencyResult(jr, task.Name, *result, upstream)
	*result = EnforceExpectedOutputs(in.dslVersion(), task, *result)
	if result.Status != apiv1.ResultSuccess {
		result.WorkspaceRevision = nil
	}
	if task.Type == apiv1.TaskDeterministic &&
		result.Status == apiv1.ResultSuccess &&
		result.WorkspaceRevision != nil {
		current := &in.workspaceRevision
		if tf.workspaceRevision != nil {
			current = tf.workspaceRevision
		}
		if err := r.acceptTaskWorkspaceRevision(ctx, jr, task.Name, attempt, class, in,
			current, tf.repoRef, result.WorkspaceRevision); err != nil {
			return err
		}
	}
	return nil
}

func taskAttemptClass(attempt, startAttempt int32, firstClass, nextClass journal.AttemptClass) journal.AttemptClass {
	switch {
	case attempt == startAttempt && firstClass != "":
		return firstClass
	case attempt == startAttempt && startAttempt > 1:
		return journal.AttemptInfra
	case attempt > startAttempt:
		return nextClass
	default:
		return ""
	}
}

func applyTaskUsageBudget(
	limits apiv1.Limits,
	usage *attemptUsageCollector,
	totals *stageUsageTotals,
	result *apiv1.ResultEnvelope,
	dispatchErr *error,
) {
	attemptUsage, usageReported := usage.snapshot()
	budget := StageUsageBudget{totals: totals}
	budget.Apply(limits, attemptUsage, usageReported, result, dispatchErr)
}

func (r *Runner) settledParallelBranchResult(
	ctx context.Context,
	branch branchState,
	history []journal.Event,
	baseCompleted stageOutputs,
	in StartInput,
	index int,
) (*parallelBranchResult, string, *parallelTaskTerminal, *parallelGateTerminal, error) {
	lastStage, lastResult, _ := lastFinishedSubject(history)
	terminalTarget, terminalTask, terminalGate := parallelBranchTerminal(history, in.Machine)
	branchInput, err := r.restoreWorkspaceRevision(ctx, in, history)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("runner: reconstruct settled parallel branch %d workspace revision: %w", branch.id, err)
	}
	var repoRef *apiv1.RepoRef
	if !reposEqual(&branchInput.RepoRef, &in.RepoRef) {
		repoRef = &branchInput.RepoRef
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
		workspaceRevision: branchInput.workspaceRevision,
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
