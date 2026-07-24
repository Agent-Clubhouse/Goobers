package vnext

import (
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/boundedwait"
)

// CheckStageTimeoutCoherence reports statically bounded waits that can outlive
// the stage executing them. Dynamic duration inputs are left alone because
// their runtime value cannot be proven unsafe from the workflow definition.
func CheckStageTimeoutCoherence(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.Run == nil {
			continue
		}
		if _, dynamicKind := task.InputsFrom[boundedwait.InputKind]; dynamicKind {
			continue
		}
		switch task.Inputs[boundedwait.InputKind] {
		case "", boundedwait.KindShell, boundedwait.KindCIPoll:
		default:
			continue
		}

		stageTimeout, ok := effectiveStageTimeout(task)
		if !ok {
			continue
		}
		wait, source, ok := boundedPollWait(task)
		if !ok {
			continue
		}
		wait, clamp, ok := effectivePollWait(task, wait, stageTimeout)
		if !ok || wait < stageTimeout {
			continue
		}
		if clamp != "" {
			source += ", " + clamp
		}
		problems = append(problems, fmt.Sprintf(
			"task %q inputs.%s has effective bounded wait %s (%s), meeting or exceeding its effective stage timeout %s; the executor can terminate the stage before the wait finishes and before the stage writes a result",
			task.Name, boundedwait.InputPollTimeout, wait, source, stageTimeout,
		))
	}
	return problems
}

func effectiveStageTimeout(task apiv1.Task) (time.Duration, bool) {
	limits := TaskLimits(task)
	if limits.MaxDurationSeconds > 0 {
		return time.Duration(limits.MaxDurationSeconds) * time.Second, true
	}
	if !isShellStage(task) {
		return 0, false
	}
	if _, dynamic := task.InputsFrom[boundedwait.InputTimeout]; dynamic {
		return 0, false
	}
	value := task.Inputs[boundedwait.InputTimeout]
	if value == "" {
		return boundedwait.DefaultTimeout, true
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, false
	}
	return timeout, true
}

func boundedPollWait(task apiv1.Task) (time.Duration, string, bool) {
	if _, dynamic := task.InputsFrom[boundedwait.InputPollTimeout]; dynamic {
		return 0, "", false
	}
	value := task.Inputs[boundedwait.InputPollTimeout]
	if value == "" {
		if !isCIPollStage(task) {
			subcommand, ok := goobersSubcommand(task)
			if !ok || subcommand != "merge-queue-poll" {
				return 0, "", false
			}
		}
		return boundedwait.DefaultPollTimeout, fmt.Sprintf("default %s", boundedwait.DefaultPollTimeout), true
	}
	wait, err := time.ParseDuration(value)
	if err != nil || wait <= 0 {
		return 0, "", false
	}
	return wait, fmt.Sprintf("declared %q", value), true
}

func effectivePollWait(task apiv1.Task, wait, stageTimeout time.Duration) (time.Duration, string, bool) {
	if isCIPollStage(task) {
		if budget := boundedwait.CIPollBudget(stageTimeout); wait > budget {
			return budget, fmt.Sprintf("clamped from %s by ci-poll", wait), true
		}
		return wait, "", true
	}
	subcommand, ok := goobersSubcommand(task)
	if !ok || subcommand != "merge-queue-poll" {
		return wait, "", true
	}
	clampStageTimeout, ok := mergeQueueCommandStageTimeout(task)
	if !ok {
		return 0, "", false
	}
	if budget := boundedwait.MergeQueuePollBudget(clampStageTimeout); wait > budget {
		return budget, fmt.Sprintf("clamped from %s by merge-queue-poll", wait), true
	}
	return wait, "", true
}

// mergeQueueCommandStageTimeout mirrors cmd/goobers.stageTimeout: the
// subprocess sees inputs.timeout, not the task's canonical timeoutSeconds.
func mergeQueueCommandStageTimeout(task apiv1.Task) (time.Duration, bool) {
	if _, dynamic := task.InputsFrom[boundedwait.InputTimeout]; dynamic {
		return 0, false
	}
	value := task.Inputs[boundedwait.InputTimeout]
	if timeout, err := time.ParseDuration(value); err == nil && timeout > 0 {
		return timeout, true
	}
	return boundedwait.DefaultTimeout, true
}

func isCIPollStage(task apiv1.Task) bool {
	return task.Type == apiv1.TaskDeterministic &&
		task.Inputs[boundedwait.InputKind] == boundedwait.KindCIPoll
}
