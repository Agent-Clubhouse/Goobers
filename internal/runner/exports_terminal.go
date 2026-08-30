package runner

import (
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// BlockedReason and ParseBlockedBy expose the two derivations the runner
// applies to a blocked stage's result envelope when it builds a
// BlockedOutcome.
//
// They are exported for the daemon's engine terminal-hook frame (decision 005
// D1): an engine-driven run's terminal hooks fire in cmd/goobers, from the
// engine.RunResult the workflow returned rather than from the runner's own
// walk, and a BlockedOutcome assembled with a different reason string or a
// different blockedBy parse would give an operator a DIFFERENT blocked record
// for the same stage outcome depending only on which driver executed it. The
// two drivers must be reading the identical fields out of the identical
// envelope, which means one implementation, not two.
//
// Thin wrappers rather than renames so the runner's own call sites keep
// reading as the local helpers they are.
func BlockedReason(result apiv1.ResultEnvelope) string { return blockedReason(result) }

// ParseBlockedBy parses the blockedBy stage output into issue references.
func ParseBlockedBy(outputs map[string]any) []string { return parseBlockedBy(outputs) }

// OutputRateLimitReset parses the executor.OutputRateLimitReset RFC3339
// timestamp a stage writes into its declared result file on a rate-limited
// failure (#614), reporting false when the key is absent, not a string, or
// not parseable.
//
// Exported for the daemon's live-journal rate-limit observer (decision 005
// D1): an engine-driven stage's rate-limited failure reaches this process as
// a journal event rather than as a live result envelope, and the observer
// must recover the reset instant with the SAME parse the runner uses. A
// second parse — a different accepted layout, or a different treatment of a
// zero value — would mean the scheduler parked for a different window
// depending only on which driver executed the stage.
func OutputRateLimitReset(outputs map[string]any) (time.Time, bool) {
	return outputRateLimitReset(outputs)
}
