package recovery

import (
	"fmt"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// AbandonedEvent records an operator's explicit decision about one exact
// published snapshot. The caller must authenticate the operator, hold the
// terminal run's lifecycle lock, and compare the current inventory record
// before appending this event durably. This event alone deletes no data.
func AbandonedEvent(record Record) (journal.Event, error) {
	event, err := RetainedEvent(record)
	if err != nil {
		return journal.Event{}, err
	}
	event.Runner["operation"] = "recovery-abandoned"
	event.Runner["actor"] = "cli"
	return event, nil
}

// ExplicitlyAbandoned accepts only host-written operator evidence for the
// exact record. Stage annotations cannot authorize deletion. Any subsequent
// change, including retention renewal, requires a new abandonment decision.
// Callers must independently establish terminality and hold lifecycle locks.
func ExplicitlyAbandoned(events []journal.Event, record Record) (bool, error) {
	if err := record.Validate(); err != nil {
		return false, err
	}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.RunID != record.RunID ||
			event.Runner["operation"] != "recovery-abandoned" || event.Runner["actor"] != "cli" {
			continue
		}
		if _, emitted := event.Runner[livejournal.EmitKeyRunnerField]; emitted {
			continue
		}
		observed, err := recordFromEvent(event)
		if err != nil {
			return false, fmt.Errorf("invalid recovery abandonment evidence: %w", err)
		}
		if sameAbandonmentRecord(observed, record) {
			return true, nil
		}
	}
	return false, nil
}

func sameAbandonmentRecord(observed, current Record) bool {
	observedCreatedAt, observedRetainUntil := observed.CreatedAt, observed.RetainUntil
	currentCreatedAt, currentRetainUntil := current.CreatedAt, current.RetainUntil
	observed.CreatedAt, observed.RetainUntil = current.CreatedAt, current.RetainUntil
	return observed == current &&
		observedCreatedAt.Equal(currentCreatedAt) &&
		observedRetainUntil.Equal(currentRetainUntil)
}
