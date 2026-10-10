package localscheduler

import (
	"context"
	"errors"
	"time"

	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// SourceTrigger retains the host's accepted signal, schedule or counted-worker provenance.
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

// SourceQueue commits starts under tickMu without reentering scheduler/catalog locks.
type SourceQueue interface {
	ScheduleCursor(context.Context, WorkflowEntry, time.Time, bool, time.Time) (time.Time, bool, error)
	AcceptSchedule(context.Context, WorkflowEntry, time.Time, time.Time, bool) error
	AcceptSignal(context.Context, []WorkflowEntry, string, string, string, *webhookhttp.Delivery, time.Time) ([]string, error)
}

// WithSourceQueue installs durable acceptance for a configured host.
func WithSourceQueue(queue SourceQueue) Option { return func(s *Scheduler) { s.sourceQueue = queue } }

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
