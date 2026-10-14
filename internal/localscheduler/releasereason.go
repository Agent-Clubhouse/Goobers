package localscheduler

import (
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
)

// Stable codes for releases whose cause the ledger or the run journal decides.
const (
	// ReleaseCodeLeaseExpired is a lease reaped by RecoverExpired.
	ReleaseCodeLeaseExpired = "lease_expired"
	// ReleaseCodeRunJournalUnavailable is a terminal release whose run
	// journal could not be read, so the cause is deliberately Unknown.
	ReleaseCodeRunJournalUnavailable = "run_journal_unavailable"
	// ReleaseCodeRunNotTerminal is a run-wide release of a run that has not
	// recorded a terminal outcome.
	ReleaseCodeRunNotTerminal = "run_not_terminal"
)

// StallSweptReason is the reason RecoverExpired records: the holder stopped
// renewing (a stalled or crashed run) and the sweep reclaimed the item.
func StallSweptReason() claimrelease.Reason {
	return claimrelease.StallSwept.Because(ReleaseCodeLeaseExpired, "").Normalize()
}

// TerminalReleaseReason derives why a run's claims end from its own journal:
// the terminal phase, the run.finished work disposition (no-work) and the
// durably recorded terminal cause (stage, stable code, bounded message).
// findRunDir resolves runID's directory (instance.Layout.FindRunDir). A run
// whose journal cannot be located or read is reported as Unknown with a code
// saying so, never guessed.
func TerminalReleaseReason(findRunDir func(string) (string, error), runID string) claimrelease.Reason {
	events, err := runEvents(findRunDir, runID)
	if err != nil {
		return claimrelease.Unknown.Because(ReleaseCodeRunJournalUnavailable, "").Normalize()
	}
	return ReleaseReasonFromRunEvents(events)
}

func runEvents(findRunDir func(string) (string, error), runID string) ([]journal.Event, error) {
	if findRunDir == nil {
		return nil, errors.New("no run directory resolver")
	}
	runDir, err := findRunDir(runID)
	if err != nil {
		return nil, err
	}
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		return nil, err
	}
	events, err := reader.Events()
	if err != nil {
		return nil, fmt.Errorf("read run journal: %w", err)
	}
	return events, nil
}

// ReleaseReasonFromRunEvents maps one run journal snapshot to a release
// reason. The latest run.finished decides; a later run.resumed (or any other
// reopening event) means the run is live again, which a terminal release must
// not misreport as its last outcome.
func ReleaseReasonFromRunEvents(events []journal.Event) claimrelease.Reason {
	phase := journal.PhaseFromEvents(events)
	if phase == journal.PhaseRunning {
		return claimrelease.Unknown.Because(ReleaseCodeRunNotTerminal, "").Normalize()
	}
	finished, _ := lastRunFinished(events)
	cause, causeErr := journal.TerminalCauseFromEvents(events)
	if causeErr != nil {
		cause = nil
	}
	reason := claimrelease.Reason{Disposition: terminalDisposition(phase, finished.Disposition, cause)}
	if cause != nil {
		if cause.SelectorKind == "stage" || cause.SelectorKind == "gate" {
			reason.Stage = cause.Selector
		}
		reason.Code = cause.Code
		reason.Summary = cause.Message
	}
	return reason.Normalize()
}

func lastRunFinished(events []journal.Event) (journal.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunFinished && events[i].KnownSchema() {
			return events[i], true
		}
	}
	return journal.Event{}, false
}

func terminalDisposition(phase journal.RunPhase, work string, cause *journal.TerminalCause) claimrelease.Disposition {
	switch phase {
	case journal.PhaseCompleted:
		if work == journal.RunDispositionNoWork {
			return claimrelease.NoWork
		}
		return claimrelease.Completed
	case journal.PhaseFailed:
		return claimrelease.Failed
	case journal.PhaseEscalated:
		return claimrelease.Escalated
	case journal.PhaseAborted:
		if cause != nil && cause.Classification == journal.TerminalDefinedAbort {
			// The workflow itself chose its abort branch: it gave the item
			// back, nobody cancelled it.
			return claimrelease.Abandoned
		}
		return claimrelease.Cancelled
	default:
		return claimrelease.Unknown
	}
}
