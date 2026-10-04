package localscheduler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// Validate checks the closed schedule/signal/worker variants retained by the host.
func (source SourceTrigger) Validate() error {
	if source.WorkerKind != "" {
		return source.validateWorker()
	}
	if !source.ObservedAt.IsZero() || source.ObservedCount != 0 || source.WorkerOrdinal != 0 {
		return errors.New("localscheduler: worker evidence requires a worker source")
	}
	if source.Signal != "" {
		if len(source.Signal) > 256 || len(source.Ref) > 512 || source.Ref == "" || !source.ScheduledFrom.IsZero() || !source.ScheduledAt.IsZero() {
			return errors.New("localscheduler: invalid queued signal")
		}
		for _, value := range []string{source.Signal, source.Ref} {
			for _, r := range value {
				if r < 0x20 || r == 0x7f {
					return errors.New("localscheduler: invalid signal metadata")
				}
			}
		}
		return nil
	}
	if source.Ref != "" || source.Webhook || source.ScheduledFrom.IsZero() || !source.ScheduledAt.After(source.ScheduledFrom) {
		return errors.New("localscheduler: invalid queued schedule")
	}
	return nil
}

// Trigger returns the exact journal provenance expected from an accepted start.
func (source SourceTrigger) Trigger(workflow string) journal.Trigger {
	if source.WorkerKind != "" {
		return journal.Trigger{Kind: journal.TriggerItem, Ref: workflow}
	}
	if source.Signal != "" {
		return journal.Trigger{Kind: journal.TriggerSignal, Ref: source.Ref}
	}
	return journal.Trigger{Kind: journal.TriggerSchedule, Ref: workflow}
}
func matchesQueuedSignal(entry WorkflowEntry, name string, delivery *webhookhttp.Delivery) bool {
	if !slices.Contains(entry.Signals, name) {
		return false
	}
	if delivery == nil || delivery.RepositoryOwner == "" || delivery.RepositoryName == "" {
		return true
	}
	return entry.RepoRef.Provider == apiv1.ProviderGitHub && strings.EqualFold(entry.RepoRef.Owner, delivery.RepositoryOwner) && strings.EqualFold(entry.RepoRef.Name, delivery.RepositoryName)
}

func (s *Scheduler) dispatchPreparedSource(executionCtx context.Context, entry WorkflowEntry, runID string, source SourceTrigger, now time.Time) (string, error) {
	if err := source.Validate(); err != nil {
		return "", err
	}
	s.mu.Lock()
	current := s.workflows[entryIdentity(entry)]
	s.mu.Unlock()
	if source.WorkerKind != "" {
		return s.dispatchPreparedWorker(executionCtx, entry, current, runID, source, now)
	}
	var indexes []int
	reason := "signal"
	if source.Signal != "" {
		if !slices.Contains(entry.Signals, source.Signal) || !slices.Contains(current.Signals, source.Signal) {
			return "", errors.New("localscheduler: queued signal subscription no longer configured")
		}
		if source.Webhook {
			reason = "webhook delivery: " + strings.TrimPrefix(source.Signal, "github-webhook:")
		}
	} else {
		if len(entry.Schedules) == 0 || len(current.Schedules) == 0 {
			return "", errors.New("localscheduler: queued schedule no longer configured")
		}
		indexes = dueScheduleIndexes(entry.Schedules, source.ScheduledFrom, source.ScheduledAt)
		reason = fireReason(Tick(TriggerState{Workflow: entry.Workflow, Schedules: entry.Schedules, LastEval: source.ScheduledFrom}, source.ScheduledAt), journal.TriggerSchedule)
		if entry.PollFallbackCause != "" {
			reason = "polling fallback: " + entry.PollFallbackCause + "; " + reason
		}
	}
	id, admitted, why := s.dispatch(executionCtx, entry, now, source.Trigger(entry.Workflow), reason, indexes, source.Webhook, false, runID)
	if !admitted {
		return "", &TriggerRejectedError{Workflow: entry.Workflow, Reason: why}
	}
	return id, nil
}
