package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type parallelStageRecovery struct {
	parent     bool
	task       *apiv1.ResultEnvelope
	gate       *gate.Result
	gateEvent  *journal.Event
	attempt    int32
	class      journal.AttemptClass
	committed  bool
	accounting *resumeRetryAccounting
	child      *resumeContext
}

// Restore only the current branch window. A durable wait (including a wait
// whose continuation was published before a crash) is not a failed attempt.
// A replacement invocation already started after that continuation follows
// the existing interruption and external-mutation checks.
func recoverParallelStage(writer *branchJournal, machine *workflow.Machine, state string, last apiv1.ResultEnvelope, history []journal.Event) (parallelStageRecovery, error) {
	restored := parallelStageRecovery{attempt: 1}
	task, isTask := machine.Task(state)
	if isTask {
		if child, ok := recoverChildTaskContext(history, state); ok {
			if child.childWaitErr != nil {
				return restored, child.childWaitErr
			}
			restored.child = child
			if !child.childWaitRunning {
				restored.attempt, restored.class = int32(child.attempt)+1, child.class
				restored.accounting = &resumeRetryAccounting{policyAttempts: child.childWait.PolicyAttempts, infrastructureFailures: child.childWait.InfrastructureFailures, replacementConsumesPolicy: child.class != journal.AttemptInfra}
				return restored, nil
			}
		}
	}
	boundary, ok := lastParallelBoundary(history)
	if !ok || boundary.Stage != state && boundary.Gate != state {
		return restored, nil
	}
	if isTask {
		switch {
		case boundary.Type == journal.EventStageFinished && boundary.Stage == state && !isInterruptedAttemptMarker(boundary):
			restored.task = &last
		case boundary.Type == journal.EventStageStarted && boundary.Stage == state:
			var err error
			restored.parent, err = parallelParentRecoveryAvailable(writer, task, history)
			if err != nil {
				return restored, err
			}
			if err := restoreInterruptedParallelTask(writer, machine, task, boundary, history, &restored); err != nil {
				return restored, err
			}
		}
	} else if _, isGate := machine.Gate(state); isGate && boundary.Type == journal.EventGateEvaluated && boundary.Gate == state {
		gr := gateResultFromEvent(boundary)
		restored.gate, restored.gateEvent = &gr, &boundary
	}
	return restored, nil
}

func restoreInterruptedParallelTask(writer *branchJournal, machine *workflow.Machine, task apiv1.Task, boundary journal.Event, history []journal.Event, restored *parallelStageRecovery) error {
	attempt := boundary.Attempt
	if attempt == 0 {
		attempt = 1
	}
	errorDetail := &journal.ErrorDetail{Code: interruptedAttemptErrorCode, Message: "attempt was in flight when the runner was interrupted"}
	runnerDetail := map[string]any{interruptedAttemptMarkerKey: true}
	if !restored.parent && task.Type == apiv1.TaskAgentic {
		limits, err := workflow.TaskLimits(machine, task)
		if err != nil {
			return err
		}
		if usageBudgetConfigured(limits) {
			interrupted := interruptedStageBudgetFailure(limits)
			restored.task = &interrupted
			errorDetail, runnerDetail = errorDetailFrom(interrupted), nil
		}
	}
	if restored.parent {
		if err := refuseInterruptedMutation(history, task.Name, attempt); err != nil {
			return err
		}
	} else if err := appendInterruptedAttemptClosure(writer, history, task.Name, attempt, errorDetail, runnerDetail, restored.task == nil); err != nil {
		return err
	}
	if restored.task == nil {
		restored.attempt, restored.class = int32(attempt)+1, journal.AttemptInfra
		restored.committed = infraFailedAttemptCommittedWork(history, task.Name, attempt)
		restored.accounting = &resumeRetryAccounting{
			policyAttempts: policyAttemptsBefore(history, task.Name, attempt), infrastructureFailures: infrastructureFailuresBefore(history, task.Name, attempt), replacementConsumesPolicy: boundary.AttemptClass != journal.AttemptInfra,
		}
	}
	return nil
}

func applyChildTaskResume(frame *taskFrame, child *resumeContext) {
	if child == nil {
		return
	}
	frame.childWaitResume, frame.childWaitCompletion = child.childWait, child.childWaitCompletion
	frame.childWaitAttempt, frame.childWaitClass = child.attempt, child.class
}

// Keep the exact verified host recovery receipt alive until runTask restores
// its checkout and probes accepted child work. A generic failed-attempt marker
// would consume this receipt before that probe could recover the durable wait.
func parallelParentRecoveryAvailable(writer *branchJournal, task apiv1.Task, history []journal.Event) (bool, error) {
	if task.ChildWorkflows == nil {
		return false, nil
	}
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		return false, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return false, err
	}
	_, found, err := selectParentRecovery(reader, history, identity.RunID, task.Name, writer.branch)
	return found, err
}
