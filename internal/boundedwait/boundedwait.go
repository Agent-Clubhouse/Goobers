// Package boundedwait defines the duration contract shared by stage executors,
// built-in commands, and static workflow validation.
package boundedwait

import "time"

const (
	InputKind        = "kind"
	KindShell        = "shell"
	KindCIPoll       = "ci-poll"
	InputTimeout     = "timeout"
	InputPollTimeout = "pollTimeoutSeconds"

	DefaultTimeout     = 10 * time.Minute
	DefaultPollTimeout = 30 * time.Minute

	ciPollResultMargin      = time.Second
	mergeQueuePollMinMargin = time.Minute
)

// CIPollBudget leaves time for a typed timeout result to cross the stage
// boundary before the runner's enclosing wall-clock limit expires.
func CIPollBudget(stage time.Duration) time.Duration {
	margin := ciPollResultMargin
	if margin >= stage {
		margin = stage / 10
	}
	if budget := stage - margin; budget > 0 {
		return budget
	}
	return stage / 2
}

// MergeQueuePollBudget leaves time for merge-queue-poll to exit cleanly and
// write its result file before the shell executor terminates the stage.
func MergeQueuePollBudget(stage time.Duration) time.Duration {
	margin := stage / 10
	if margin < mergeQueuePollMinMargin {
		margin = mergeQueuePollMinMargin
	}
	if budget := stage - margin; budget > 0 {
		return budget
	}
	return stage / 2
}
