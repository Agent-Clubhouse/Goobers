// Package boundedwait defines the duration contract shared by stage executors,
// built-in commands, and static workflow validation.
package boundedwait

import "time"

// Input names, stage kinds, and defaults shared by bounded wait implementations.
const (
	InputKind        = "kind"
	KindShell        = "shell"
	KindCIPoll       = "ci-poll"
	InputTimeout     = "timeout"
	InputPollTimeout = "pollTimeoutSeconds"

	InputPollInterval             = "pollIntervalSeconds"
	InputPollMaxInterval          = "pollMaxIntervalSeconds"
	InputRetryFailedChecksBackoff = "retryFailedChecksBackoffSeconds"

	KindExternalTelemetry           = "external-telemetry"
	InputTelemetryWindow            = "window"
	InputTelemetryFreshness         = "freshness"
	InputTelemetryQueryTimeout      = "queryTimeout"
	InputTelemetryQueryRetryBackoff = "queryRetryBackoff"

	DefaultTimeout     = 10 * time.Minute
	DefaultPollTimeout = 30 * time.Minute

	ciPollResultMargin      = time.Second
	mergeQueuePollMinMargin = time.Minute
)

// CIPollDurationInputs lists the ci-poll stage inputs the executor parses
// with time.ParseDuration, despite the "Seconds" suffix on their names.
func CIPollDurationInputs() []string {
	return []string{InputPollInterval, InputPollMaxInterval, InputPollTimeout, InputRetryFailedChecksBackoff}
}

// ExternalTelemetryDurationInputs lists the external-telemetry stage inputs
// the executor parses with time.ParseDuration and requires to be positive.
func ExternalTelemetryDurationInputs() []string {
	return []string{InputTelemetryWindow, InputTelemetryFreshness, InputTelemetryQueryTimeout, InputTelemetryQueryRetryBackoff}
}

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
