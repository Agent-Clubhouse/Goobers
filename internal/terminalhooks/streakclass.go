package terminalhooks

import (
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
)

// TerminalFault is the failure-streak disposition of an escalated or aborted
// terminal, read from what the run already journaled (#5638).
type TerminalFault string

const (
	// TerminalFaultUnclassified is a terminal with no infrastructure
	// classification behind it: a genuine/work terminal.
	TerminalFaultUnclassified TerminalFault = ""
	// TerminalFaultItemInfrastructure is infrastructure that is plausibly
	// about the item: the run's own validation of it (a gate such as
	// local-gate judging a local-ci timeout infra) kept failing that way.
	// A CI suite that hangs on one item's change every time lands here, so it
	// is counted — on a separate, higher-threshold streak — rather than
	// retried forever.
	TerminalFaultItemInfrastructure TerminalFault = "item-infrastructure"
	// TerminalFaultSubstrate is infrastructure that is not about the item: a
	// dispatch that could not run (credentials, git, network, harness
	// startup), a gate evaluator that could not run, or an engine workflow
	// the daemon had to terminate because no worker would poll it. Nothing
	// the item contains causes these, so they must not park it at all.
	TerminalFaultSubstrate TerminalFault = "substrate"
)

// JournaledTerminalFault classifies an escalated or aborted terminal from the
// classifications already journaled for the run rather than re-deriving them
// from a code (#5638). It is the escalated/aborted counterpart of
// FailedOutcome.FaultClass: a failed terminal carries its class on the
// outcome, because the Failed handler runs before run.finished exists; an
// escalated or aborted terminal is notified after run.finished, so its class
// is in the journal.
//
//   - A gate that escalated on an exhausted infrastructure repass budget is
//     item infrastructure: the gate judged the item's own validation infra
//     until the budget ran out.
//   - Any other infrastructure-failure terminal cause (an engine workflow the
//     daemon terminated) is substrate.
//   - A watchdog interruption (run_duration_exceeded, run_stalled) takes the
//     class of the most recent classified decision before the terminal, with
//     no stage or gate result since: a gate retry decision journaled
//     retryFailureClass "infra" is item infrastructure (local-gate routing a
//     timed-out local-ci back to local-ci, and the run then expiring during
//     that retry); a dispatch or gate-evaluator failure journaled "infra" is
//     substrate.
//
// The local runner journals the watchdog's code as the terminal cause. The
// Temporal driver cannot: the stall sweep stops an engine run with
// CancelWorkflow, which carries no reason, so the engine journals every
// cancellation as run_canceled. engineInterruption is the sweep's own record
// of why it cancelled — RunStalledErrorCode or RunDurationExceededErrorCode,
// or "" for none or for an operator's cancel — and is honored only on a
// run_canceled terminal. An operator's cancel is never a watchdog
// interruption.
func JournaledTerminalFault(events []journal.Event, engineInterruption string) TerminalFault {
	finished := -1
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunFinished {
			finished = i
			break
		}
	}
	if finished < 0 {
		return TerminalFaultUnclassified
	}
	cause := events[finished].TerminalCause
	// A failed terminal's cause defaults to infrastructure-failure whatever
	// failed, so it carries no classification here; FailedOutcome owns it.
	if cause == nil || cause.Phase == journal.PhaseFailed || cause.Phase == journal.PhaseCompleted {
		return TerminalFaultUnclassified
	}
	if cause.Classification == journal.TerminalInfrastructureFailure {
		if cause.Code == gate.ReasonInfrastructureBudgetExhausted {
			return TerminalFaultItemInfrastructure
		}
		return TerminalFaultSubstrate
	}
	code := cause.Code
	if code == runsemantics.RunCanceledErrorCode && isWatchdogInterruption(engineInterruption) {
		code = engineInterruption
	}
	if !isWatchdogInterruption(code) {
		return TerminalFaultUnclassified
	}
	return interruptedRetryFault(events[:finished])
}

// EndedByCancellation reports whether the run's latest terminal is a
// run_canceled one — the only terminal JournaledTerminalFault consults an
// engine interruption for, so a caller need not look one up otherwise.
func EndedByCancellation(events []journal.Event) bool {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunFinished {
			cause := events[i].TerminalCause
			return cause != nil && cause.Code == runsemantics.RunCanceledErrorCode
		}
	}
	return false
}

func isWatchdogInterruption(code string) bool {
	return code == runsemantics.RunDurationExceededErrorCode || code == runsemantics.RunStalledErrorCode
}

// interruptedRetryFault walks back from the terminal to the most recent
// classified decision. Starting the retried attempt is part of the retry, so
// stage.started, a resume boundary, and an error event that carries no retry
// classification (the watchdog's own interruption record, the engine's
// cancellation cause) are passed over; any stage or gate result means the run
// had moved on from the retry.
func interruptedRetryFault(events []journal.Event) TerminalFault {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Schema != "" && !e.KnownSchema() {
			continue // A future schema's events are not ours to interpret.
		}
		switch e.Type {
		case journal.EventRunFinished, journal.EventStageFinished, journal.EventGateEvaluated, journal.EventGateOverridden:
			return TerminalFaultUnclassified
		case journal.EventRunnerAnnotation:
			if e.Runner["kind"] != runsemantics.RetryDecisionKind {
				continue
			}
			if e.Runner[runsemantics.RetryFailureClassKey] == string(journal.AttemptInfra) {
				return TerminalFaultItemInfrastructure
			}
			return TerminalFaultUnclassified
		case journal.EventError:
			class, ok := e.Runner[runsemantics.RetryFailureClassKey].(string)
			if !ok {
				continue
			}
			if class == string(journal.AttemptInfra) {
				return TerminalFaultSubstrate
			}
			return TerminalFaultUnclassified
		}
	}
	return TerminalFaultUnclassified
}
