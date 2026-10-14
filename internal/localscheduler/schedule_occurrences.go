package localscheduler

import (
	"context"
	"errors"
	"time"
)

const maxScheduleOccurrencesPerSweep = 100

// OccurrenceSourceQueue owns independent schedule positions and pinned fires.
type OccurrenceSourceQueue interface {
	ScheduleOccurrences(context.Context, WorkflowEntry, time.Time, bool, time.Time) ([]time.Time, bool, error)
	AcceptOccurrence(context.Context, WorkflowEntry, int, time.Time, time.Time, time.Time) error
}

func (s *Scheduler) queueAllOccurrences(ctx context.Context, candidate *tickCandidate, now time.Time) (bool, bool) {
	entry := candidate.entry
	queue, ok := s.sourceQueue.(OccurrenceSourceQueue)
	if !ok {
		s.sourceQueueError(entry, errors.New("localscheduler: occurrence queue unavailable"))
		return true, false
	}
	identity := entryIdentity(entry)
	s.mu.Lock()
	state := s.triggers[identity]
	pending := s.pendingScheduleDemand[identity]
	s.mu.Unlock()
	positions, legacy, err := queue.ScheduleOccurrences(ctx, entry, state.LastEval, pending.remaining > 0 || pending.repoll, now)
	if err != nil {
		s.sourceQueueError(entry, err)
		return true, false
	}
	if legacy {
		return false, false
	} // Existing coalesced custody transfers once before the vector is initialized.
	candidate.scheduleRemaining = 0
	candidate.schedulePollDue = false
	candidate.scheduleDemand = false
	advanced, more, err := captureScheduleOccurrences(ctx, queue, entry, positions, now)
	candidate.catchUpWake = more
	if err != nil {
		s.sourceQueueError(entry, err)
	}
	s.mu.Lock()
	if len(positions) > 0 {
		state.LastEval = positions[0]
		for _, position := range positions[1:] {
			if position.Before(state.LastEval) {
				state.LastEval = position
			}
		}
		s.triggers[identity] = state
	}
	delete(s.pendingScheduleDemand, identity)
	s.mu.Unlock()
	if pending.remaining > 0 || pending.repoll {
		s.persistScheduleDemand(identity, false)
	}
	if demand, ok := s.sourceQueue.(DemandSourceQueue); ok {
		candidate.demandWake = true
		s.loadQueuedScheduleDemand(ctx, demand, candidate)
	}
	return true, advanced
}

func captureScheduleOccurrences(ctx context.Context, queue OccurrenceSourceQueue, entry WorkflowEntry, positions []time.Time, now time.Time) (bool, bool, error) {
	advanced := false
	for count := 0; count <= maxScheduleOccurrencesPerSweep; count++ {
		index, next, err := nextScheduleOccurrence(entry.Schedules, positions, now)
		if err != nil {
			return advanced, true, err
		}
		if index < 0 {
			return advanced, false, nil
		}
		if count == maxScheduleOccurrencesPerSweep {
			return advanced, true, nil
		}
		if err = ctx.Err(); err != nil {
			return advanced, true, err
		}
		if err = queue.AcceptOccurrence(ctx, entry, index, positions[index], next, now); err != nil {
			return advanced, true, err
		}
		positions[index] = next
		advanced = true
	}
	return advanced, true, nil
}

func nextScheduleOccurrence(schedules []Schedule, positions []time.Time, now time.Time) (int, time.Time, error) {
	if len(positions) != len(schedules) || len(positions) < 1 || len(positions) > 1000 {
		return -1, time.Time{}, errors.New("localscheduler: invalid schedule positions")
	}
	index := -1
	var next time.Time
	for i, schedule := range schedules {
		at := schedule.Next(positions[i])
		if at.IsZero() {
			continue
		}
		if !at.After(positions[i]) {
			return -1, time.Time{}, errors.New("localscheduler: schedule did not advance")
		}
		if at.After(now) {
			continue
		}
		if index < 0 || at.Before(next) {
			index, next = i, at
		}
	}
	return index, next, nil
}

func (source SourceTrigger) validateScheduleDeclaration() error {
	if source.ScheduleDeclaration == 0 {
		return nil
	}
	if source.ScheduleDeclaration < 1 || source.ScheduleDeclaration > 1000 || source.Signal != "" || source.WorkerKind != "" || !source.ObservedAt.IsZero() || source.ObservedCount != 0 || source.WorkerOrdinal != 0 || !source.ScheduleWindowFrom.IsZero() || source.ScheduleFireCount != 0 {
		return errors.New("localscheduler: invalid schedule declaration provenance")
	}
	return nil
}

func (source SourceTrigger) occurrenceEvaluation(schedules []Schedule) (ScheduleWindowEvaluation, error) {
	index := source.ScheduleDeclaration - 1
	if index < 0 || index >= len(schedules) || !schedules[index].Next(source.ScheduledFrom).Equal(source.ScheduledAt) {
		return ScheduleWindowEvaluation{}, errors.New("localscheduler: captured occurrence differs from archived declaration")
	}
	return ScheduleWindowEvaluation{Tick: TickResult{Fire: true, MissedTicks: 1, LastEval: source.ScheduledAt}, From: source.ScheduledFrom, Indexes: []int{index}}, nil
}

// ReasonScheduleBackoff holds retained nominal occurrences until idle backoff clears.
const ReasonScheduleBackoff = "schedule backoff"

func (s *Scheduler) occurrenceAdmissionIndexes(archived, current WorkflowEntry, declaration int, now time.Time) ([]int, error) {
	identity, err := scheduleIdentity(archived.Schedules[declaration-1])
	if err != nil {
		return nil, err
	}
	var indexes []int
	for i, schedule := range current.Schedules {
		candidate, err := scheduleIdentity(schedule)
		if err != nil {
			return nil, err
		}
		if candidate == identity {
			indexes = append(indexes, i)
		}
	}
	if blocked, reason := s.scheduleBackedOff(entryIdentity(current), current, indexes, now); blocked {
		return nil, &TriggerRejectedError{Workflow: current.Workflow, Reason: ReasonScheduleBackoff + ": " + reason}
	}
	return indexes, nil
}
