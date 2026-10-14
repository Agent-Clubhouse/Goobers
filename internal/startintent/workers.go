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

// PendingWorkers accounts for ordinary, signal, and generated-child starts in
// this exact configured workflow bucket. Live identities are counted by the host.
func (s *Sources) PendingWorkers(ctx context.Context, entry localscheduler.WorkflowEntry, active []string) (int, error) {
	return s.Queue.PendingWorkflowStarts(ctx, triggerqueue.WorkflowPendingLimit{Gaggle: entry.Gaggle, Workflow: entry.Workflow, ActiveRunIDs: active})
}

// AcceptWorkers records a bounded provider observation and worker ordinals.
// These are run starts only: item eligibility and claims remain in the workflow.
func (s *Sources) AcceptWorkers(ctx context.Context, entry localscheduler.WorkflowEntry, observation localscheduler.WorkerObservation) error {
	if observation.At.IsZero() || observation.Count < 1 || observation.Count > triggerqueue.MaxSourceStarts || observation.Eligible < observation.Count || observation.MaxPending < observation.Count {
		return errors.New("startintent: invalid worker observation")
	}
	key := sourceHash("worker-observation", entry.Gaggle, entry.Workflow, observation.Kind, observation.At.UTC().Format(time.RFC3339Nano))
	fingerprint := sourceHash(key, strconv.Itoa(observation.Eligible), strconv.Itoa(observation.Count))
	if prior, err := s.Queue.SourceReceipt(ctx, key); err == nil {
		if prior.Actor != "scheduler" || prior.Fingerprint != fingerprint {
			return triggerqueue.ErrConflict
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	source := localscheduler.SourceTrigger{WorkerKind: observation.Kind, ObservedAt: observation.At.UTC(), ObservedCount: observation.Eligible, WorkerOrdinal: 1}
	raw, release, err := s.pin(ctx, entry, source)
	if err != nil {
		return err
	}
	defer release()
	envelope, err := Parse(raw)
	if err != nil {
		return err
	}
	batch := triggerqueue.SourceBatch{Key: key, Actor: "scheduler", Fingerprint: fingerprint, PendingLimit: &triggerqueue.WorkflowPendingLimit{Gaggle: entry.Gaggle, Workflow: entry.Workflow, MaxPending: observation.MaxPending, ActiveRunIDs: observation.ActiveRunIDs}}
	for ordinal := 1; ordinal <= observation.Count; ordinal++ {
		envelope.Source.WorkerOrdinal = ordinal
		payload, err := envelope.Marshal()
		if err != nil {
			return err
		}
		batch.Starts = append(batch.Starts, triggerqueue.SourceStart{Payload: payload})
	}
	_, _, err = s.Queue.AcceptSource(ctx, batch, observation.At)
	return err
}
