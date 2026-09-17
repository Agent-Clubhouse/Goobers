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
// schedule interval (#1868). It runs after dispatch, over every entry —
// including the ones this tick skipped silently (auth circuit open, placement
// refused) — because those are precisely the states in which a lane can stay
// dead indefinitely with nothing in the journal to say so.
//
// One event is emitted per stall episode rather than per tick: the flag is
// cleared as soon as the trigger fires again and the silence falls back under
// the threshold, so a chronically wedged lane does not flood the journal.
//
// The signal is "the trigger loop stopped evaluating", not "no trigger.fired
// landed": LastEval advances whenever a schedule was due and fired, even if
// the resulting dispatch was refused.
//
// That leaves the second shape, which journalCapacityStarvation covers: a lane
// that keeps firing on time and is refused admission every single time.
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

// capacityRefusal is how long dispatch has been refused for capacity, and the
// most recent reason given.
type capacityRefusal struct {
	Since  time.Time
	Reason string
}

// recordDispatchOutcome maintains the capacity-refusal window for one workflow.
//
// An admitted dispatch clears it: capacity existed, so whatever was being
// waited on arrived. Only TRANSIENT refusals open or extend the window — the
// same set TriggerRejectedError.Transient() names, which are precisely the
// refusals justified by "capacity that is about to exist". Budget, quota and
// open-PR-cap refusals are deliberate throttles: a workflow an operator has
// stopped with `workflowBudgets: {wf: 0}` is behaving exactly as configured and
// must never alarm.
func (s *Scheduler) recordDispatchOutcome(identity WorkflowIdentity, admitted bool, reason string, now time.Time) {
	transient := (&TriggerRejectedError{Reason: reason}).Transient()
	s.mu.Lock()
	defer s.mu.Unlock()
	if admitted || !transient {
		delete(s.capacityRefusals, identity)
		delete(s.capacityStarvedNotified, identity)
		return
	}
	// Keep the first refusal's timestamp: the window measures the episode, not
	// the latest tick within it.
	refusal, open := s.capacityRefusals[identity]
	if !open {
		refusal.Since = now
	}
	refusal.Reason = reason
	s.capacityRefusals[identity] = refusal
}

// journalCapacityStarvation reports a scheduled workflow that keeps firing on
// time but has not been admitted for triggerStallMultiple of its own interval.
//
// # Why #1868's check does not see this
//
// journalTriggerStalls asks whether the trigger loop is still evaluating. For
// this shape it is: the schedule fires every interval, LastEval advances every
// time, and the stall check is satisfied on every tick. What never happens is a
// dispatch.
//
// The old claim was that such a lane "journals tick.skipped instead, so it is
// not silent either way". That is true and useless. `tick.skipped` with reason
// `conditions: max-parallel` is exactly what a HEALTHY busy workflow emits
// while its runs are in flight, so the wedged lane and the working one produce
// the same journal line, tick after tick, forever.
//
// That is how #5272 happened. backlog-curation — maxConcurrentRuns: 1, and the
// only promoter of goobers:approved -> goobers:ready — had one run wedged in
// `running`. Every subsequent tick fired, was refused max-parallel, and skipped.
// The instance had ZERO claimable work for most of a day while the failure rate
// sat at 0% and every health signal stayed green, because nothing distinguishes
// "busy" from "wedged" without asking how long it has been busy.
//
// A transient refusal is a promise that capacity is about to exist. Five of the
// workflow's own intervals is long enough to say the promise was not kept.
func (s *Scheduler) journalCapacityStarvation(entries []WorkflowEntry, now time.Time) {
	for _, entry := range entries {
		if len(entry.Schedules) == 0 {
			continue
		}
		identity := entryIdentity(entry)

		s.mu.Lock()
		refusal, open := s.capacityRefusals[identity]
		notified := s.capacityStarvedNotified[identity]
		s.mu.Unlock()
		if !open {
			continue
		}

		interval, ok := scheduleInterval(entry.Schedules, refusal.Since)
		if !ok {
			continue
		}
		refused := now.Sub(refusal.Since)
		if refused < interval*triggerStallMultiple {
			continue
		}
		if notified {
			continue
		}

		s.mu.Lock()
		s.capacityStarvedNotified[identity] = true
		s.mu.Unlock()
		s.journalEvent(journal.Event{
			Type:     journal.EventWorkflowStarved,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason: fmt.Sprintf(
				"scheduled trigger has fired but dispatched no run for %s, over %dx its %s schedule interval (refused: %s)",
				refused.Round(time.Second), triggerStallMultiple, interval, refusal.Reason,
			),
		})
	}
}
