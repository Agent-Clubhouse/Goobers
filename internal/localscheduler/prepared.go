package localscheduler

import (
	"errors"
	"reflect"
)

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
	prepared.ProviderAuth = current.ProviderAuth
	return prepared, nil
}
