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

// Validate checks the closed signal provenance retained by the host.
func (source SourceTrigger) Validate() error {
	if source.Signal == "" || len(source.Signal) > 256 || source.Ref == "" || len(source.Ref) > 512 {
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

// Trigger returns exact journal provenance for accepted signals.
func (source SourceTrigger) Trigger(string) journal.Trigger {
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
	if !slices.Contains(entry.Signals, source.Signal) || !slices.Contains(current.Signals, source.Signal) {
		return "", errors.New("localscheduler: queued signal subscription no longer configured")
	}
	reason := "signal"
	if source.Webhook {
		reason = "webhook delivery: " + strings.TrimPrefix(source.Signal, "github-webhook:")
	}
	id, admitted, why := s.dispatch(executionCtx, entry, now, source.Trigger(entry.Workflow), reason, nil, source.Webhook, false, runID)
	if !admitted {
		return "", &TriggerRejectedError{Workflow: entry.Workflow, Reason: why}
	}
	return id, nil
}
