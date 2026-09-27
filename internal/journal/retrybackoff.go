package journal

import "time"

// RetryBackoffKind records an actual retry timer, not an inferred retry count.
const RetryBackoffKind = "retry-backoff.v1"

// RetryBackoffResetKind invalidates process-local timers during crash resume.
const RetryBackoffResetKind = "retry-backoff-reset.v1"

// RetryBackoffEvent describes the timer's scheduling decision. The observed
// clock is explicit because a local journal stamps its append time separately.
func RetryBackoffEvent(stage string, attempt int, driver string, class AttemptClass, observedAt, deadline time.Time) Event {
	return Event{Type: EventRunnerAnnotation, Stage: stage, Attempt: attempt, Runner: map[string]any{
		"kind": RetryBackoffKind, "driver": driver, "retryClass": string(class),
		"observedAt": observedAt.UTC().Format(time.RFC3339Nano), "deadline": deadline.UTC().Format(time.RFC3339Nano),
	}}
}
