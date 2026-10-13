package startintent

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// CaptureDemand pins a due fire before observing provider demand.
func (s *Sources) CaptureDemand(ctx context.Context, entry localscheduler.WorkflowEntry, before, after time.Time) error {
	raw, release, err := s.pin(ctx, entry, localscheduler.SourceTrigger{ScheduledFrom: before.UTC(), ScheduledAt: after.UTC()})
	if err != nil {
		return err
	}
	defer release()
	revision, err := localscheduler.ScheduleRevision(entry.Schedules)
	if err != nil {
		return err
	}
	scope := scheduleScope(entry)
	id := sourceHash("schedule-demand", scope, revision, before.UTC().Format(time.RFC3339Nano), after.UTC().Format(time.RFC3339Nano))
	return s.Queue.CaptureScheduleDemand(ctx, id, triggerqueue.SourceAdvance{Scope: scope, Revision: revision, Before: before, After: after}, raw, after)
}

// LoadDemand uses the captured counter only while sizing. Sealed counts need
// no provider re-read; dispatch still validates current eligibility normally.
func (s *Sources) LoadDemand(ctx context.Context, entry localscheduler.WorkflowEntry) (*localscheduler.QueuedScheduleDemand, func(), error) {
	record, err := s.Queue.ScheduleDemand(ctx, scheduleScope(entry))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	envelope, err := Parse(record.Payload)
	if err != nil {
		return nil, nil, err
	}
	if envelope.Target.Gaggle != entry.Gaggle || envelope.Target.Workflow != entry.Workflow {
		return nil, nil, errors.New("startintent: retained demand crosses configured scope")
	}
	if envelope.Source == nil || envelope.Source.ScheduledAt.IsZero() {
		return nil, nil, errors.New("startintent: invalid retained demand source")
	}
	demand := &localscheduler.QueuedScheduleDemand{ID: record.ID, Source: *envelope.Source, Count: record.Count, Queued: record.Queued}
	if record.Count >= 0 {
		return demand, nil, nil
	}
	if s.Build == nil {
		return nil, nil, errors.New("startintent: pinned schedule counter unavailable")
	}
	prepared, err := s.Build(ctx, envelope.Target)
	if err != nil {
		return nil, nil, err
	}
	if prepared.Release == nil {
		return nil, nil, errors.New("startintent: pinned schedule counter has no archive lease")
	}
	if prepared.Entry.ScheduleDemandCounter == nil {
		if prepared.Release != nil {
			prepared.Release()
		}
		return nil, nil, errors.New("startintent: accepted schedule has no sizing counter")
	}
	demand.Pinned = &prepared.Entry
	return demand, prepared.Release, nil
}

// ObserveDemand seals the original count, including explicit no-work.
func (s *Sources) ObserveDemand(ctx context.Context, id string, count int) error {
	return s.Queue.ObserveScheduleDemand(ctx, id, count)
}

// AcceptDemandWorkers transfers exact consecutive ordinals transactionally.
func (s *Sources) AcceptDemandWorkers(ctx context.Context, entry localscheduler.WorkflowEntry, demand localscheduler.QueuedScheduleDemand, observation localscheduler.WorkerObservation) error {
	if observation.Count < 1 || observation.Count > triggerqueue.MaxSourceStarts || observation.MaxPending < observation.Count {
		return errors.New("startintent: invalid schedule worker batch")
	}
	key := sourceHash("schedule-workers", demand.ID, strconv.Itoa(demand.Queued), strconv.Itoa(observation.Count))
	if prior, err := s.Queue.SourceReceipt(ctx, key); err == nil {
		if prior.Actor != "scheduler" || prior.Fingerprint != key {
			return triggerqueue.ErrConflict
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	record, err := s.Queue.ScheduleDemand(ctx, scheduleScope(entry))
	if err != nil {
		return err
	}
	if record.ID != demand.ID || record.Count != demand.Count || record.Queued != demand.Queued {
		return triggerqueue.ErrTransition
	}
	batch := triggerqueue.SourceBatch{Key: key, Actor: "scheduler", Fingerprint: key, Demand: &triggerqueue.DemandTransfer{ID: demand.ID, Before: demand.Queued, After: demand.Queued + observation.Count}, PendingLimit: &triggerqueue.WorkflowPendingLimit{Gaggle: entry.Gaggle, Workflow: entry.Workflow, MaxPending: observation.MaxPending, ActiveRunIDs: observation.ActiveRunIDs}}
	envelope, err := Parse(record.Payload)
	if err != nil {
		return err
	}
	for i := 0; i < observation.Count; i++ {
		envelope.Source.ScheduleCount = demand.Count
		envelope.Source.ScheduleOrdinal = demand.Queued + i + 1
		raw, err := envelope.Marshal()
		if err != nil {
			return err
		}
		batch.Starts = append(batch.Starts, triggerqueue.SourceStart{Payload: raw})
	}
	_, _, err = s.Queue.AcceptSource(ctx, batch, observation.At)
	return err
}

// Untransferred demand keeps its captured definition even before any run exists.
func retainedScheduleDemandGenerations(ctx context.Context, queue *triggerqueue.Store) (map[string]bool, error) {
	pins := map[string]bool{}
	var after string
	for {
		page, err := queue.ScheduleDemandPage(ctx, after, 100)
		if err != nil {
			return nil, err
		}
		for _, demand := range page {
			envelope, err := Parse(demand.Payload)
			if err != nil {
				return nil, err
			}
			pins[envelope.Target.ConfigGeneration] = true
			after = demand.ID
		}
		if len(page) < 100 {
			return pins, nil
		}
	}
}
