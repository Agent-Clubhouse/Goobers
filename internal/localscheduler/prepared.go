package localscheduler

import (
	"context"
	"errors"
	"reflect"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TriggerPrepared dispatches an already verified archived workflow through the
// ordinary scheduler. It never installs an alias or consults a mutable starter.
// The same current workflow owns capacity/provider/hour/day budgets. Its current
// eligibility and exact repository target must still permit this execution.
func (s *Scheduler) TriggerPrepared(ctx context.Context, prepared WorkflowEntry, runID string, trigger journal.Trigger, now time.Time) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if !apiv1.ValidRunID(runID) || prepared.Gaggle == "" || prepared.Workflow == "" || prepared.Starter == nil || now.IsZero() || s.log == nil {
		return "", errors.New("localscheduler: invalid prepared execution")
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
	return s.triggerWorkflow(ctx, prepared, now, trigger, "accepted pinned event", false, runID)
}

// PreparedEntry checks current scope and eligibility without replacing archived
// execution definitions. Recovery uses it before restoring the same run.
func (s *Scheduler) PreparedEntry(prepared WorkflowEntry) (WorkflowEntry, error) {
	s.mu.Lock()
	current, known := s.workflows[entryIdentity(prepared)]
	s.mu.Unlock()
	if !known {
		return WorkflowEntry{}, errors.New("localscheduler: prepared workflow no longer configured")
	}
	if !reflect.DeepEqual(current.RepoRef, prepared.RepoRef) {
		return WorkflowEntry{}, errors.New("localscheduler: prepared repository differs from current scope")
	}
	if reason, refused := s.permanentDispatchRefusal(current); refused {
		return WorkflowEntry{}, &TriggerRejectedError{Workflow: prepared.Workflow, Reason: reason}
	}
	prepared.Readiness = current.Readiness
	return prepared, nil
}
