package localscheduler

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// SourceTrigger retains the host's schedule/signal/worker provenance with a queued start.
// ScheduledFrom/At reproduce the captured due indexes against the pinned definition.
type SourceTrigger struct {
	ScheduleCount   int       `json:"scheduleCount,omitempty"`
	ScheduleOrdinal int       `json:"scheduleOrdinal,omitempty"`
	WorkerKind      string    `json:"workerKind,omitempty"`
	ObservedAt      time.Time `json:"observedAt,omitempty"`
	ObservedCount   int       `json:"observedCount,omitempty"`
	WorkerOrdinal   int       `json:"workerOrdinal,omitempty"`
	Signal          string    `json:"signal,omitempty"`
	Ref             string    `json:"ref,omitempty"`
	Webhook         bool      `json:"webhook,omitempty"`
	ScheduledFrom   time.Time `json:"scheduledFrom,omitempty"`
	ScheduledAt     time.Time `json:"scheduledAt,omitempty"`
}

// SourceQueue commits starts without executing them. Methods run under tickMu
// and must never call back into scheduler admission or acquire applied-catalog locks.
type SourceQueue interface {
	ScheduleCursor(context.Context, WorkflowEntry, time.Time, bool) (time.Time, bool, error)
	AcceptSchedule(context.Context, WorkflowEntry, time.Time, time.Time, bool) error
	AcceptSignal(context.Context, []WorkflowEntry, string, string, string, *webhookhttp.Delivery, time.Time) ([]string, error)
}

// WithSourceQueue transfers source custody before execution to the shared ledger.
func WithSourceQueue(queue SourceQueue) Option { return func(s *Scheduler) { s.sourceQueue = queue } }

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
	before, adoptLegacy, err := s.sourceQueue.ScheduleCursor(ctx, entry, state.LastEval, pending.remaining > 0 || pending.repoll)
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

// QueueSignal durably accepts a caller-keyed named signal. Returned IDs identify
// queued starts, not already running executions; the caller can observe each receipt.
func (s *Scheduler) QueueSignal(ctx context.Context, key, name string, now time.Time) ([]string, error) {
	return s.queueSignal(ctx, key, name, name, nil, now)
}

// AcceptWebhook implements webhook's durable acknowledgement extension. The
// authenticated delivery ID owns the pinned recipient set across daemon restarts.
func (s *Scheduler) AcceptWebhook(ctx context.Context, delivery webhookhttp.Delivery, now time.Time) ([]string, error) {
	if s.sourceQueue == nil {
		return nil, errors.New("localscheduler: durable webhook queue unavailable")
	}
	return s.queueSignal(ctx, delivery.ID, webhookhttp.SignalName(delivery.Event), webhookhttp.TriggerRef(delivery), &delivery, now)
}
func (s *Scheduler) queueSignal(ctx context.Context, key, name, ref string, delivery *webhookhttp.Delivery, now time.Time) ([]string, error) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	if s.sourceQueue == nil {
		return nil, errors.New("localscheduler: durable signal queue unavailable")
	}
	var entries []WorkflowEntry
	for _, entry := range s.entriesByPollPriority() {
		if !matchesQueuedSignal(entry, name, delivery) {
			continue
		}
		if delivery != nil {
			if blocked, _ := s.webhookBackedOff(entryIdentity(entry), entry.WebhookBackoff, now); blocked {
				continue
			}
		}
		entries = append(entries, entry)
	}
	ids, err := s.sourceQueue.AcceptSignal(ctx, entries, key, name, ref, delivery, now)
	if err == nil {
		for _, entry := range entries {
			s.resetIdleBackoff(entryIdentity(entry))
		}
	}
	return ids, err
}
