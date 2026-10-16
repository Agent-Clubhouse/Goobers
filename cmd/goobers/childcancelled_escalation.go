package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Escalation has already ended this execution, but its child invocation still
// awaits a human. Family cancellation settles that invocation without reopening
// or rewriting the escalated journal. captureTerminal owns physical custody
// and verifies repository writer joins before storing this result.
func (l *queuedChildLauncher) cancelledEscalationInput(ctx context.Context, ref childExecutionRef, rd *journal.Reader, events []journal.Event) (childworkflow.TerminalResultInput, bool, error) {
	var input childworkflow.TerminalResultInput
	cancelled, err := l.queue.ChildCancellationTime(ctx, ref.Child.Identity)
	if errors.Is(err, sql.ErrNoRows) {
		return input, false, nil
	}
	if err != nil {
		return input, false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventRunFinished {
			continue
		}
		if event.Status != string(journal.PhaseEscalated) || event.Time.IsZero() {
			return input, false, errors.New("cancelled child escalation lacks terminal evidence")
		}
		// Both execution termination and the cancellation request must have
		// happened. These durable clocks make capture replay deterministic.
		if cancelled.Before(event.Time) {
			cancelled = event.Time
		}
		refs, _, err := childResultEvidence(rd, events[:i+1])
		if err != nil {
			return input, false, err
		}
		return childworkflow.TerminalResultInput{State: triggerqueue.ChildCancelled, FinishedAt: cancelled, Summary: "Child workflow cancelled while awaiting human intervention", References: refs}, true, nil
	}
	return input, false, errors.New("cancelled child escalation lacks a terminal event")
}
