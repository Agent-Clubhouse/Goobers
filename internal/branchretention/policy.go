// Package branchretention defines the conservative journal and item authority
// for deleting terminal local run branches.
package branchretention

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func Settled(events []journal.Event) bool {
	if journal.ParkedAtGate(events) {
		return false
	}
	switch journal.PhaseFromEvents(events) {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted:
		return true
	}
	return false
}

// TerminalAt never substitutes a commit date or mutable file modification time.
func TerminalAt(identity journal.RunIdentity, events []journal.Event, now time.Time) (time.Time, error) {
	if !Settled(events) {
		return time.Time{}, nil
	}
	phase := journal.PhaseFromEvents(events)
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventRunFinished || event.Branch != 0 {
			continue
		}
		if event.Status != string(phase) || event.Time.IsZero() || event.Time.After(now) || event.Time.Before(identity.StartedAt) {
			break
		}
		return event.Time, nil
	}
	return time.Time{}, fmt.Errorf("retention: run %s has no trustworthy terminal timestamp", identity.RunID)
}

// Parked recognizes provider state and the provider-visible lifecycle labels.
func Parked(state string, labels []string) bool {
	for _, value := range append([]string{state}, labels...) {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.NewReplacer("_", "-", " ", "-").Replace(value)
		value = strings.TrimPrefix(value, "goobers:")
		value = strings.TrimPrefix(value, "status:")
		switch value {
		case "needs-human", "escalated", "blocked", "blocked-on-sibling", "paused", "parked":
			return true
		}
	}
	return false
}

// ClaimedItemIDs is only a completeness check; repository routing must use the
// durable selection-time item-repo record, never current gaggle configuration.
func ClaimedItemIDs(identity journal.RunIdentity, events []journal.Event) []string {
	ids := map[string]bool{}
	if identity.Trigger.Kind == journal.TriggerItem && identity.Trigger.Ref != "" {
		ids[identity.Trigger.Ref] = true
	}
	for _, event := range events {
		if event.Type != journal.EventStageFinished {
			continue
		}
		id, idOK := event.Outputs["id"].(string)
		_, titleOK := event.Outputs["title"].(string)
		if idOK && titleOK && id != "" {
			ids[id] = true
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
