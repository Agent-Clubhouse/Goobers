package localscheduler

import (
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// triggerStallMultiple is how many of its own schedule intervals a scheduled
// workflow may go without firing before the scheduler journals a
// workflow.starved diagnostic (#1868). A silently dead trigger — wedged, auth
// circuit open, placement refused, or stalled for a reason nobody has
// diagnosed yet — is otherwise invisible: the only signal is the absence of
// trigger.fired events, which an operator can find only by hand-counting the
// journal. Five intervals is wide enough that ordinary jitter, a slow tick, or
// a single long-running dispatch never trips it.
const triggerStallMultiple = 5

// scheduleInterval estimates the shortest gap between consecutive fires across
// schedules, measured forward from after. Schedules whose fires are not
// uniformly spaced (e.g. "0 9 * * 1-5") yield the specific gap that follows
// after, which is the right baseline for "this trigger is overdue" — the
// comparison is against a multiple of it, not against an exact fire time.
// Reports false when no schedule yields two future fires.
func scheduleInterval(schedules []Schedule, after time.Time) (time.Duration, bool) {
	var shortest time.Duration
	found := false
	for _, sched := range schedules {
		first := sched.Next(after)
		if first.IsZero() {
			continue
		}
		second := sched.Next(first)
		if second.IsZero() {
			continue
		}
		interval := second.Sub(first)
		if interval <= 0 {
			continue
		}
		if !found || interval < shortest {
			shortest = interval
			found = true
		}
	}
	return shortest, found
}

// journalTriggerStalls records one workflow.starved event per scheduled
// workflow whose trigger has not fired for triggerStallMultiple of its own
// schedule interval (#1868). It runs before every other tick decision, over
// every entry — including the ones the tick then skips silently (auth circuit
// open, placement refused) — because those are precisely the states in which
// a lane can stay dead indefinitely with nothing in the journal to say so.
//
// One event is emitted per stall episode rather than per tick: the flag is
// cleared as soon as the trigger fires again and the silence falls back under
// the threshold, so a chronically wedged lane does not flood the journal.
func (s *Scheduler) journalTriggerStalls(entries []WorkflowEntry, now time.Time) {
	for _, entry := range entries {
		if len(entry.Schedules) == 0 {
			continue
		}
		identity := entryIdentity(entry)

		s.mu.Lock()
		// LastEval doubles as "last fired": Tick advances it only when a
		// schedule was due and fired, and leaves it untouched otherwise.
		lastFire := s.triggers[identity].LastEval
		notified := s.triggerStallNotified[identity]
		s.mu.Unlock()
		if lastFire.IsZero() {
			continue
		}

		interval, ok := scheduleInterval(entry.Schedules, lastFire)
		if !ok {
			continue
		}
		silent := now.Sub(lastFire)
		if silent < interval*triggerStallMultiple {
			if notified {
				s.mu.Lock()
				delete(s.triggerStallNotified, identity)
				s.mu.Unlock()
			}
			continue
		}
		if notified {
			continue
		}

		s.mu.Lock()
		s.triggerStallNotified[identity] = true
		s.mu.Unlock()
		s.journalEvent(journal.Event{
			Type:     journal.EventWorkflowStarved,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason: fmt.Sprintf(
				"scheduled trigger has not fired for %s, over %dx its %s schedule interval",
				silent.Round(time.Second), triggerStallMultiple, interval,
			),
		})
	}
}
