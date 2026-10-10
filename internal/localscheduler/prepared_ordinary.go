package localscheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// PreparedTriggerOptions retains ordinary trigger semantics without letting a
// queued request choose execution pins or alter the current capacity policy.
type PreparedTriggerOptions struct {
	Force       bool
	SourceRun   string
	PullRequest int
}

// TriggerPreparedOrdinary dispatches an archived manual/priority/PR definition
// through normal admission. Targeted PR provider validation uses admission's
// context; execution continues on the daemon's context after the caller leaves.
func (s *Scheduler) TriggerPreparedOrdinary(admission, execution context.Context, prepared WorkflowEntry, runID string, options PreparedTriggerOptions, now time.Time) (string, error) {
	if admission.Err() != nil {
		return "", admission.Err()
	}
	if !apiv1.ValidRunID(runID) || prepared.Starter == nil || now.IsZero() || s.log == nil {
		return "", errors.New("localscheduler: invalid prepared ordinary execution")
	}
	if options.PullRequest < 0 || (options.Force && (options.SourceRun != "" || options.PullRequest != 0)) || (options.SourceRun != "" && options.PullRequest != 0) {
		return "", errors.New("localscheduler: incompatible prepared trigger options")
	}
	// Keep current eligibility stable until admission, including bounded PR reads.
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	if err := admission.Err(); err != nil {
		return "", err
	}
	var err error
	prepared, err = s.PreparedEntry(prepared)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	_, active := s.admittedRuns[runID]
	_, reconciled := s.reconciledRuns[runID]
	s.mu.Unlock()
	if active || reconciled {
		return "", errors.New("localscheduler: prepared run already owned")
	}

	trigger, reason, err := s.prepareOrdinaryTrigger(admission, prepared, options)
	if err != nil {
		return "", err
	}
	return s.triggerWorkflow(execution, prepared, now, trigger, reason, options.Force, runID)
}

func (s *Scheduler) prepareOrdinaryTrigger(ctx context.Context, entry WorkflowEntry, options PreparedTriggerOptions) (journal.Trigger, string, error) {
	if options.SourceRun != "" {
		return journal.Trigger{Kind: journal.TriggerSignal, Ref: "priority-re-tick:" + options.SourceRun}, "priority re-tick requested by run " + options.SourceRun, nil
	}
	if options.PullRequest == 0 {
		return journal.Trigger{Kind: journal.TriggerManual, Ref: entry.Workflow}, "manual", nil
	}
	signal := webhookhttp.SignalName("pull_request")
	// Both captured and currently configured subscriptions must permit the PR.
	s.mu.Lock()
	current := s.workflows[entryIdentity(entry)]
	s.mu.Unlock()
	if !slices.Contains(entry.Signals, signal) || !slices.Contains(current.Signals, signal) {
		return journal.Trigger{}, "", fmt.Errorf("localscheduler: workflow %q is not subscribed to signal %q", entry.Workflow, signal)
	}
	if s.targetedPRValidator != nil {
		if err := s.targetedPRValidator(ctx, entry, options.PullRequest); err != nil {
			return journal.Trigger{}, "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return journal.Trigger{}, "", err
	}
	if deadline, ok := ctx.Deadline(); ok && !s.now().Before(deadline) {
		return journal.Trigger{}, "", context.DeadlineExceeded
	}
	return journal.Trigger{Kind: journal.TriggerSignal, Ref: webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: options.PullRequest})}, "signal", nil
}
