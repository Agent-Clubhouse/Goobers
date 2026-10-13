package localscheduler

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// QueuedScheduleDemand is a host-owned pinned fire, never an item assignment.
// Count -1 requires sizing; a sealed count survives restart without re-polling.
type QueuedScheduleDemand struct {
	ID            string
	Source        SourceTrigger
	Count, Queued int
	Pinned        *WorkflowEntry
}

// DemandSourceQueue retains a fire before polling and atomically transfers its
// consecutive worker ordinals into the same queue used by ordinary starts.
// Implementations must not acquire applied-catalog or scheduler locks.
type DemandSourceQueue interface {
	WorkerSourceQueue
	CaptureDemand(context.Context, WorkflowEntry, time.Time, time.Time) error
	LoadDemand(context.Context, WorkflowEntry) (*QueuedScheduleDemand, func(), error)
	ObserveDemand(context.Context, string, int) error
	AcceptDemandWorkers(context.Context, WorkflowEntry, QueuedScheduleDemand, WorkerObservation) error
}

func (s *Scheduler) queueConfiguredSchedule(ctx context.Context, candidate *tickCandidate, now time.Time) (bool, bool) {
	if candidate.entry.ScheduleDemandCounter == nil {
		handled, advanced := s.queuePlainSchedule(ctx, candidate.entry, now)
		if handled {
			candidate.scheduleRemaining = 0
			candidate.schedulePollDue = false
			candidate.scheduleDemand = false
		}
		return handled, advanced
	}
	queue, ok := s.sourceQueue.(DemandSourceQueue)
	if !ok {
		return false, false
	}
	candidate.demandWake = true
	candidate.scheduleRemaining = 0
	candidate.schedulePollDue = false
	candidate.scheduleDemand = false
	entry := candidate.entry
	identity := entryIdentity(entry)
	s.mu.Lock()
	state := s.triggers[identity]
	pending := s.pendingScheduleDemand[identity]
	s.mu.Unlock()
	before, legacy, err := s.sourceQueue.ScheduleCursor(ctx, entry, state.LastEval, pending.remaining > 0 || pending.repoll, now)
	if err != nil {
		s.sourceQueueError(entry, err)
		return true, false
	}
	result := Tick(TriggerState{Workflow: entry.Workflow, Schedules: entry.Schedules, LastEval: before}, now)
	if legacy && now.After(before) {
		result.Fire = true
		result.LastEval = now
	}
	if result.Fire {
		blocked, reason := s.scheduleBackedOff(identity, entry, dueScheduleIndexes(entry.Schedules, before, now), now)
		if blocked {
			s.journalEvent(journal.Event{Type: journal.EventTickSkipped, Workflow: entry.Workflow, Gaggle: entry.Gaggle, Reason: reason})
			err = s.sourceQueue.AcceptSchedule(ctx, entry, before, result.LastEval, false)
		} else {
			err = queue.CaptureDemand(ctx, entry, before, result.LastEval)
		}
		if err != nil {
			s.sourceQueueError(entry, err)
			return true, false
		}
		before = result.LastEval
	}
	s.mu.Lock()
	s.triggers[identity] = TriggerState{Workflow: entry.Workflow, Schedules: entry.Schedules, LastEval: before}
	delete(s.pendingScheduleDemand, identity)
	s.mu.Unlock()
	if pending.remaining > 0 || pending.repoll {
		s.persistScheduleDemand(identity, false)
	}
	candidate.scheduleRemaining = 0
	candidate.schedulePollDue = false
	candidate.scheduleDemand = false
	s.loadQueuedScheduleDemand(ctx, queue, candidate)
	return true, !before.Equal(state.LastEval)
}

func (s *Scheduler) loadQueuedScheduleDemand(ctx context.Context, queue DemandSourceQueue, candidate *tickCandidate) {
	demand, release, err := queue.LoadDemand(ctx, candidate.entry)
	if err != nil {
		s.sourceQueueError(candidate.entry, err)
		return
	}
	if release != nil {
		candidate.scheduleDemandLease = release
	}
	if demand == nil {
		candidate.demandWake = false
		return
	}
	if demand.Pinned != nil {
		if _, err = s.PreparedEntry(*demand.Pinned); err != nil {
			s.sourceQueueError(candidate.entry, err)
			return
		}
		candidate.entry.ScheduleDemandCounter = demand.Pinned.ScheduleDemandCounter
	}
	candidate.queuedDemand = demand
	candidate.schedule = Tick(TriggerState{Workflow: candidate.entry.Workflow, Schedules: candidate.entry.Schedules, LastEval: demand.Source.ScheduledFrom}, demand.Source.ScheduledAt)
	candidate.scheduleEnqueuedAt = demand.Source.ScheduledAt
	candidate.scheduleRemaining = max(0, demand.Count-demand.Queued)
	candidate.scheduleDemand = true
	candidate.schedulePollDue = demand.Count < 0
}
func releaseScheduleDemands(candidates []*tickCandidate) {
	for _, candidate := range candidates {
		if candidate.scheduleDemandLease != nil {
			candidate.scheduleDemandLease()
		}
	}
}

func (s *Scheduler) applyQueuedDemand(poll demandPoll, snapshot demandSnapshot) bool {
	d := poll.candidate.queuedDemand
	if !poll.schedule || d == nil {
		return false
	}
	if !snapshot.observed && snapshot.ready == 0 {
		return true
	}
	queue := s.sourceQueue.(DemandSourceQueue)
	// Poll context is already bounded. Persisting its observation must succeed
	// before workers can be accepted, including a no-work observation.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := queue.ObserveDemand(ctx, d.ID, snapshot.ready); err != nil {
		s.sourceQueueError(poll.candidate.entry, err)
		return true
	}
	d.Count = snapshot.ready
	poll.candidate.demandWake = d.Count > d.Queued
	poll.candidate.scheduleRemaining = max(0, d.Count-d.Queued)
	return true
}

func (s *Scheduler) queueDemandWorkers(ctx context.Context, candidates []*tickCandidate, now time.Time) {
	queue, ok := s.sourceQueue.(DemandSourceQueue)
	if !ok {
		return
	}
	defer s.updateDemandWake(candidates)
	for _, candidate := range candidates {
		if candidate.queuedDemand == nil {
			continue
		}
		if candidate.scheduleRemaining > 0 {
			s.queueDemandCandidate(ctx, queue, candidate, now)
		}
		// Even failed admission preserves durable custody and cannot launch directly.
		candidate.scheduleRemaining = 0
		candidate.schedulePollDue = false
	}
}
func (s *Scheduler) queueDemandCandidate(ctx context.Context, queue DemandSourceQueue, candidate *tickCandidate, now time.Time) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	entry := candidate.entry
	identity := entryIdentity(entry)
	ids := s.activeWorkerIDs(identity)
	queued, err := queue.PendingWorkers(ctx, entry, ids)
	if err != nil {
		s.sourceQueueError(entry, err)
		return
	}
	limit := max(1, int(entry.Readiness.MaxConcurrentRuns)) - s.conditions.ActiveWorkflow(identity)
	count := min(candidate.scheduleRemaining, max(0, limit-queued), 32)
	if count == 0 {
		return
	}
	observation := WorkerObservation{At: now, Count: count, Eligible: candidate.queuedDemand.Count, MaxPending: limit, ActiveRunIDs: ids}
	if err = queue.AcceptDemandWorkers(ctx, entry, *candidate.queuedDemand, observation); err != nil {
		s.sourceQueueError(entry, err)
	} else {
		candidate.demandWake = candidate.queuedDemand.Queued+count < candidate.queuedDemand.Count
	}
}

func (s *Scheduler) updateDemandWake(candidates []*tickCandidate) {
	pending := false
	for _, candidate := range candidates {
		pending = pending || candidate.demandWake
	}
	s.mu.Lock()
	s.durableScheduleDemand = pending
	s.mu.Unlock()
}

func (source SourceTrigger) validateScheduleOrdinal() error {
	if source.ScheduleCount == 0 && source.ScheduleOrdinal == 0 {
		return nil
	}
	if source.WorkerKind != "" || source.Signal != "" || source.ScheduleCount < 1 || source.ScheduleCount > 10000 || source.ScheduleOrdinal < 1 || source.ScheduleOrdinal > source.ScheduleCount {
		return errors.New("localscheduler: invalid schedule ordinal")
	}
	return nil
}
