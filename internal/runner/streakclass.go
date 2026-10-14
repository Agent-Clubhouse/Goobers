package runner

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// JournaledTerminalFaultClass is the fault class of an escalated or aborted
// terminal, read from the classifications already journaled for the run
// rather than re-derived from a code (#5638). It is the escalated/aborted
// counterpart of FailedOutcome.FaultClass: a failed terminal carries its class
// on the outcome, because the Failed handler runs before run.finished exists;
// an escalated or aborted terminal is notified after run.finished, so its
// class is in the journal.
//
// It returns telemetry.ErrorClassInfra when:
//
//   - the terminal cause is classified infrastructure-failure, as a gate that
//     exhausted its infrastructure repass budget records it; or
//   - the terminal is a watchdog interruption (run_duration_exceeded,
//     run_stalled) of work that was retrying on an infrastructure
//     classification: the most recent classified decision before the
//     terminal is a gate retry decision or a dispatch failure journaled
//     retryFailureClass "infra", with no stage or gate result since. A gate
//     that judged a local-ci timeout infra and routed back to local-ci, with
//     the run then expiring during that retry, ends here.
//
// The local runner journals the watchdog's code as the terminal cause. The
// Temporal driver cannot: the stall sweep stops an engine run with
// CancelWorkflow, which carries no reason, so the engine journals every
// cancellation as run_canceled. engineInterruption is the sweep's own record
// of why it cancelled — runner.RunStalledErrorCode or
// RunDurationExceededErrorCode, or "" for none or for an operator's cancel —
// and is honored only on a run_canceled terminal. An operator's cancel is
// never a watchdog interruption.
//
// Anything else returns "" (unclassified): consumers charge it as work, so an
// expiry or stall during ordinary work still reaches the failure streak.
func JournaledTerminalFaultClass(events []journal.Event, engineInterruption string) telemetry.ErrorClass {
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
	code := cause.Code
	if code == RunCanceledErrorCode && isWatchdogInterruption(engineInterruption) {
		code = engineInterruption
	}
	if isWatchdogInterruption(code) && interruptedInfrastructureRetry(events[:finished]) {
		return telemetry.ErrorClassInfra
	}
	return ""
}

func isWatchdogInterruption(code string) bool {
	return code == RunDurationExceededErrorCode || code == RunStalledErrorCode
}

// interruptedInfrastructureRetry walks back from the terminal to the most
// recent classified decision. Starting the retried attempt is part of the
// retry, so stage.started, a resume boundary, and an error event that carries
// no retry classification (the watchdog's own interruption record, the
// engine's cancellation cause) are passed over; any stage or gate result
// means the run had moved on from the retry.
func interruptedInfrastructureRetry(events []journal.Event) bool {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Schema != "" && !e.KnownSchema() {
			continue // A future schema's events are not ours to interpret.
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
