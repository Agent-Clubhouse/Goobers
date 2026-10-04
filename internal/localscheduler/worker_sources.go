package localscheduler

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// WorkerObservation records a provider count, not an item assignment or claim.
// MaxPending is the current workflow capacity after accounting for live runs.
type WorkerObservation struct {
	Kind                        string
	At                          time.Time
	Eligible, Count, MaxPending int
	ActiveRunIDs                []string
}

// WorkerSourceQueue extends source capture with a transactional occupancy guard.
type WorkerSourceQueue interface {
	PendingWorkers(context.Context, WorkflowEntry, []string) (int, error)
	AcceptWorkers(context.Context, WorkflowEntry, WorkerObservation) error
}

func (source SourceTrigger) validateWorker() error {
	if source.WorkerKind != "backlog" && source.WorkerKind != "refill" {
		return errors.New("localscheduler: unknown worker source")
	}
	if source.ObservedAt.IsZero() || source.ObservedCount < 1 || source.WorkerOrdinal < 1 || source.WorkerOrdinal > source.ObservedCount {
		return errors.New("localscheduler: invalid worker observation")
	}
	if source.Signal != "" || source.Ref != "" || source.Webhook || !source.ScheduledFrom.IsZero() || !source.ScheduledAt.IsZero() {
		return errors.New("localscheduler: worker source has incompatible provenance")
	}
	return nil
}
func (s *Scheduler) dispatchPreparedWorker(ctx context.Context, entry, current WorkflowEntry, runID string, source SourceTrigger, now time.Time) (string, error) {
	if source.WorkerKind == "backlog" && (entry.BacklogCounter == nil || current.BacklogCounter == nil) {
		return "", errors.New("localscheduler: backlog worker source no longer configured")
	}
	reason := "backlog item ready"
	if source.WorkerKind == "refill" {
		if entry.RefillDemandCounter == nil || current.RefillDemandCounter == nil || current.Readiness.DesiredConcurrentRuns <= 0 {
			return "", errors.New("localscheduler: refill worker source no longer configured")
		}
		reason = refillTriggerReason
	}
	id, admitted, why := s.dispatch(ctx, entry, now, journal.Trigger{Kind: journal.TriggerItem, Ref: entry.Workflow}, reason, nil, false, false, runID)
	if !admitted {
		return "", &TriggerRejectedError{Workflow: entry.Workflow, Reason: why}
	}
	return id, nil
}

func (s *Scheduler) queueCountWorkers(ctx context.Context, candidates []*tickCandidate, now time.Time) {
	queue, ok := s.sourceQueue.(WorkerSourceQueue)
	if !ok {
		return
	}
	for _, candidate := range candidates {
		if candidate.backlogRemaining <= 0 && candidate.refillRemaining <= 0 {
			continue
		}
		s.queueCandidateWorkers(ctx, queue, candidate, now)
		// Durable source custody is the sole path for these automatic worker starts.
		candidate.backlogRemaining = 0
		candidate.refillRemaining = 0
	}
}
func (s *Scheduler) queueCandidateWorkers(ctx context.Context, queue WorkerSourceQueue, candidate *tickCandidate, now time.Time) {
	// Keep the live-count snapshot and exact active identities coherent with
	// child/continuation admission while the queue atomically checks pending work.
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	entry := candidate.entry
	identity := entryIdentity(entry)
	active := s.conditions.ActiveWorkflow(identity)
	ids := s.activeWorkerIDs(identity)
	queued, err := queue.PendingWorkers(ctx, entry, ids)
	if err != nil {
		s.sourceQueueError(entry, err)
		return
	}
	limit := max(1, int(entry.Readiness.MaxConcurrentRuns)) - active
	available := max(0, limit-queued)
	// Reserve planned scheduled starts too; their current source adapter still
	// owns its separate pending-fire custody during this bounded migration.
	available = max(0, available-candidate.scheduleRemaining)
	if available == 0 {
		return
	}
	if candidate.backlogRemaining > 0 {
		count := min(max(0, candidate.backlogRemaining-queued), available, 32)
		if count > 0 {
			observation := WorkerObservation{Kind: "backlog", At: now, Eligible: candidate.backlogRemaining, Count: count, MaxPending: max(0, limit), ActiveRunIDs: ids}
			if err = queue.AcceptWorkers(ctx, entry, observation); err != nil {
				s.sourceQueueError(entry, err)
				return
			}
			available -= count
			queued += count
		}
	}
	if candidate.refillRemaining <= 0 || available <= 0 {
		return
	}
	desiredMissing := int(entry.Readiness.DesiredConcurrentRuns) - active - queued - candidate.scheduleRemaining
	count := min(candidate.refillRemaining, available, desiredMissing, 32)
	if count <= 0 {
		return
	}
	observation := WorkerObservation{Kind: "refill", At: now, Eligible: candidate.refillEligible, Count: count, MaxPending: max(0, min(limit, int(entry.Readiness.DesiredConcurrentRuns)-active)), ActiveRunIDs: ids}
	if err = queue.AcceptWorkers(ctx, entry, observation); err != nil {
		s.sourceQueueError(entry, err)
	}
}
func (s *Scheduler) activeWorkerIDs(identity WorkflowIdentity) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, admission := range s.admittedRuns {
		if admission.identity == identity && !admission.suspended {
			ids = append(ids, id)
		}
	}
	for id, run := range s.reconciledRuns {
		if run.identity == identity && !run.suspended {
			ids = append(ids, id)
		}
	}
	return ids
}
