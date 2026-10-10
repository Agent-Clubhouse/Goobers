package localscheduler

import (
	"context"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// PreparedTriggerOptions retains ordinary trigger semantics without letting a
// queued request choose execution pins or alter the current capacity policy.
type PreparedTriggerOptions struct {
	Source    *SourceTrigger
	Force     bool
	SourceRun string
}

// TriggerPreparedOrdinary dispatches an archived manual/priority definition
// through current admission. Execution uses the daemon's lifetime context.
func (s *Scheduler) TriggerPreparedOrdinary(admission, execution context.Context, prepared WorkflowEntry, runID string, options PreparedTriggerOptions, now time.Time) (string, error) {
	if admission.Err() != nil {
		return "", admission.Err()
	}
	if !apiv1.ValidRunID(runID) || prepared.Starter == nil || now.IsZero() || s.log == nil {
		return "", errors.New("localscheduler: invalid prepared ordinary execution")
	}
	if (options.Force && options.SourceRun != "") || (options.Source != nil && (options.Force || options.SourceRun != "")) {
		return "", errors.New("localscheduler: incompatible prepared trigger options")
	}
	// Keep current eligibility stable until admission.
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

	if options.Source != nil {
		return s.dispatchPreparedSource(execution, prepared, runID, *options.Source, now)
	}
	trigger, reason := journal.Trigger{Kind: journal.TriggerManual, Ref: prepared.Workflow}, "manual"
	if options.SourceRun != "" {
		trigger = journal.Trigger{Kind: journal.TriggerSignal, Ref: "priority-re-tick:" + options.SourceRun}
		reason = "priority re-tick requested by run " + options.SourceRun
	}
	return s.triggerWorkflow(execution, prepared, now, trigger, reason, options.Force, runID)
}
