package localscheduler

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func (s *Scheduler) queuePlainSchedule(ctx context.Context, entry WorkflowEntry, now time.Time) (handled, advanced bool) {
	if s.sourceQueue == nil || entry.ScheduleDemandCounter != nil || len(entry.Schedules) == 0 {
		return false, false
	}
	identity := entryIdentity(entry)
	s.mu.Lock()
	state := s.triggers[identity]
	previous := state.LastEval
	pending := s.pendingScheduleDemand[identity]
	s.mu.Unlock()
	before, adoptLegacy, err := s.sourceQueue.ScheduleCursor(ctx, entry, state.LastEval, pending.remaining > 0 || pending.repoll, now)
	if err != nil {
		s.sourceQueueError(entry, err)
		return true, false
	}
	if !adoptLegacy && (pending.remaining > 0 || pending.repoll) {
		s.mu.Lock()
		delete(s.pendingScheduleDemand, identity)
		s.mu.Unlock()
		s.persistScheduleDemand(identity, false)
	}
	result := Tick(TriggerState{Workflow: entry.Workflow, Schedules: entry.Schedules, LastEval: before}, now)
	// Transfer a legacy outstanding single worker fire once during adoption.
	if adoptLegacy && now.After(before) {
		result.Fire = true
		result.LastEval = now
	}
	if result.Fire {
		indexes := dueScheduleIndexes(entry.Schedules, before, now)
		blocked, reason := s.scheduleBackedOff(identity, entry, indexes, now)
		if err = s.sourceQueue.AcceptSchedule(ctx, entry, before, result.LastEval, !blocked); err != nil {
			s.sourceQueueError(entry, err)
			return true, false
		}
		if blocked {
			s.journalEvent(journal.Event{Type: journal.EventTickSkipped, Workflow: entry.Workflow, Gaggle: entry.Gaggle, Reason: reason})
		}
		s.mu.Lock()
		state.LastEval = result.LastEval
		delete(s.pendingScheduleDemand, identity)
		s.mu.Unlock()
		if pending.remaining > 0 || pending.repoll {
			s.persistScheduleDemand(identity, false)
		}
	} else {
		state.LastEval = before
	}
	s.mu.Lock()
	s.triggers[identity] = state
	s.mu.Unlock()
	return true, !state.LastEval.Equal(previous)
}

func (s *Scheduler) sourceQueueError(entry WorkflowEntry, err error) {
	s.journalEvent(journal.Event{Type: journal.EventError, Workflow: entry.Workflow, Gaggle: entry.Gaggle, Error: journal.ErrorDetailFor("source_start_acceptance_failed", err)})
}

func (s *Scheduler) prepareTickSchedule(ctx context.Context, entry WorkflowEntry, now time.Time, evaluated *[]WorkflowEntry) *tickCandidate {
	identity := entryIdentity(entry)
	s.mu.Lock()
	pending := s.pendingScheduleDemand[identity]
	s.mu.Unlock()
	candidate := &tickCandidate{
		entry:              entry,
		schedule:           pending.schedule,
		scheduleRemaining:  pending.remaining,
		scheduleDemand:     pending.remaining > 0,
		schedulePollDue:    pending.repoll,
		scheduleEnqueuedAt: pending.enqueuedAt,
	}
	if pending.remaining == 0 {
		candidate.schedule = TickResult{LastEval: now}
	}
	if handled, advanced := s.queueConfiguredSchedule(ctx, candidate, now); handled {
		if advanced {
			*evaluated = append(*evaluated, entry)
		}
	} else if len(entry.Schedules) > 0 {
		// Read, evaluate, and write the trigger state under a single lock
		// acquisition. Tick is exported so a manual trigger and concurrent
		// Tick calls (e.g. overlapping Run-loop iterations) can race here;
		// dropping the lock between the read and the write let two callers
		// both read the same pre-fire TriggerState, both compute Fire=true,
		// and both dispatch the same due firing.
		// Persisting the snapshot is batched to once per tick (#6010).
		s.mu.Lock()
		ts := s.triggers[identity]
		lastEval := ts.LastEval
		dueIndexes := dueScheduleIndexes(entry.Schedules, ts.LastEval, now)
		res := Tick(ts, now)
		if res.LastEval != ts.LastEval {
			*evaluated = append(*evaluated, entry)
		}
		s.triggers[identity] = TriggerState{Workflow: entry.Workflow, Schedules: entry.Schedules, LastEval: res.LastEval}
		s.mu.Unlock()
		if res.Fire {
			candidate.schedule = res
			candidate.scheduleEnqueuedAt = oldestDueScheduleAt(entry.Schedules, lastEval, now)
			candidate.scheduleIndexes = dueIndexes
			if blocked, reason := s.scheduleBackedOff(identity, entry, dueIndexes, now); blocked {
				s.journalEvent(journal.Event{
					Type:     journal.EventTickSkipped,
					Workflow: entry.Workflow,
					Gaggle:   entry.Gaggle,
					Reason:   reason,
				})
				candidate.scheduleIndexes = nil
			} else if entry.ScheduleDemandCounter == nil {
				// Coalesces with a retained fire; scheduleDemand stays set
				// so admission consumes its marker (#6207).
				candidate.scheduleRemaining = 1
			} else {
				candidate.schedulePollDue = true
			}
		}
	}
	return candidate
}
