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

// Validate checks the closed source provenance retained by the host.
func (source SourceTrigger) Validate() error {
	if err := source.validateScheduleDeclaration(); err != nil {
		return err
	}
	if err := source.validateScheduleWindow(); err != nil {
		return err
	}
	if err := source.validateScheduleOrdinal(); err != nil {
		return err
	}
	if source.WorkerKind != "" || !source.ObservedAt.IsZero() || source.ObservedCount != 0 || source.WorkerOrdinal != 0 {
		return source.validateWorker()
	}
	if source.Signal == "" {
		if source.Ref != "" || source.Webhook || source.ScheduledFrom.IsZero() || !source.ScheduledAt.After(source.ScheduledFrom) {
			return errors.New("localscheduler: invalid queued schedule")
		}
		return nil
	}
	if !source.ScheduledFrom.IsZero() || !source.ScheduledAt.IsZero() || len(source.Signal) > 256 || source.Ref == "" || len(source.Ref) > 512 {
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

// Trigger returns exact journal provenance for the accepted source.
func (source SourceTrigger) Trigger(workflow string) journal.Trigger {
	if source.WorkerKind != "" {
		return journal.Trigger{Kind: journal.TriggerItem, Ref: workflow}
	}
	if source.Signal == "" {
		return journal.Trigger{Kind: journal.TriggerSchedule, Ref: workflow}
	}
	return journal.Trigger{Kind: journal.TriggerSignal, Ref: source.Ref}
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
		// The accepted source is validated against its archived declaration.
		// Current workflow eligibility still applies; removing future triggers
		// does not revoke a fire that already has durable custody.
		if len(entry.Schedules) == 0 {
			return "", errors.New("localscheduler: queued schedule has no archived declaration")
		}
		window, err := source.scheduleEvaluation(entry.Schedules)
		if err != nil {
			return "", err
		}
		indexes = window.Indexes
		if source.ScheduleDeclaration > 0 {
			indexes, err = s.occurrenceAdmissionIndexes(entry, current, source.ScheduleDeclaration, now)
			if err != nil {
				return "", err
			}
			// Backoff follows current declarations; execution pins remain archived.
			entry.ScheduleBackoffs = current.ScheduleBackoffs
		}
		tick := window.Tick
		reason = fireReason(tick, journal.TriggerSchedule)
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
