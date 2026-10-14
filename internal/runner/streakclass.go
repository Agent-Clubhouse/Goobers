package runner

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// JournaledTerminalFaultClass is the fault class of an escalated or aborted
// terminal, read from the classifications the runner and the engine already
// journaled for the run rather than re-derived from a code (#5638). It is the
// escalated/aborted counterpart of FailedOutcome.FaultClass: a failed terminal
// carries its class on the outcome, because the Failed handler runs before
// run.finished exists; an escalated or aborted terminal is notified after
// run.finished, so its class is in the journal.
//
// It returns telemetry.ErrorClassInfra when:
//
//   - the terminal cause is classified infrastructure-failure, as a gate that
//     exhausted its infrastructure repass budget records it; or
//   - the terminal is a watchdog interruption (run_duration_exceeded,
//     run_stalled) of work that was retrying on an infrastructure
//     classification: the most recent classified decision before the
//     interruption is a gate retry decision or a dispatch failure journaled
//     retryFailureClass "infra", with no stage or gate result since. A gate
//     that judged a local-ci timeout infra and routed back to local-ci, with
//     the run then expiring during that retry, ends here.
//
// Anything else returns "" (unclassified): consumers keep counting it, so an
// expiry or stall during ordinary work still reaches the failure streak. Both
// drivers journal the same run.finished cause and retry annotations
// (BuildTerminalCause, RetryDecisionKind), so the one reading serves both.
func JournaledTerminalFaultClass(events []journal.Event) telemetry.ErrorClass {
	finished := -1
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunFinished {
			finished = i
			break
		}
	}
	if finished < 0 {
		return ""
	}
	cause := events[finished].TerminalCause
	// A failed terminal's cause defaults to infrastructure-failure whatever
	// failed, so it carries no classification here; FailedOutcome owns it.
	if cause == nil || cause.Phase == journal.PhaseFailed || cause.Phase == journal.PhaseCompleted {
		return ""
	}
	if cause.Classification == journal.TerminalInfrastructureFailure {
		return telemetry.ErrorClassInfra
	}
	if isWatchdogInterruption(cause.Code) && interruptedInfrastructureRetry(events[:finished]) {
		return telemetry.ErrorClassInfra
	}
	return ""
}

func isWatchdogInterruption(code string) bool {
	return code == RunDurationExceededErrorCode || code == RunStalledErrorCode
}

// interruptedInfrastructureRetry walks back from the watchdog's interruption
// event to the most recent classified decision. Starting the retried attempt
// is part of the retry, so stage.started (and a resume boundary) is passed
// over; any stage or gate result means the run had moved on from the retry.
func interruptedInfrastructureRetry(events []journal.Event) bool {
	interrupted := false
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if !interrupted {
			interrupted = e.Type == journal.EventError && e.Error != nil && isWatchdogInterruption(e.Error.Code)
			continue
		}
		switch e.Type {
		case journal.EventRunFinished, journal.EventStageFinished, journal.EventGateEvaluated, journal.EventGateOverridden:
			return false
		case journal.EventRunnerAnnotation:
			if e.Runner["kind"] == RetryDecisionKind {
				return e.Runner[RetryFailureClassKey] == string(journal.AttemptInfra)
			}
		case journal.EventError:
			if class, ok := e.Runner[RetryFailureClassKey].(string); ok {
				return class == string(journal.AttemptInfra)
			}
		}
	}
	return false
}
