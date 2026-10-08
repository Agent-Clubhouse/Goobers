package localscheduler

import (
	"context"
	"errors"
)

// ChildParentSuspension retains the parent's original admission ownership while
// lending its concurrency slot back to the pool. Only a host that has joined all
// runnable branches and durably recorded the wait may request this transition.
// The original dispatch cleanup remains authoritative, including on cancellation.
type ChildParentSuspension struct {
	scheduler  *Scheduler
	runID      string
	generation uint64
	reconciled bool
}

// SuspendChildParent does not refund any rolling start budget. Multiple live
// owners are refused: suspending one branch cannot surrender siblings' capacity.
// The caller must additionally establish that its single run owner has no live
// parallel branch; scheduler owners count dispatches, not stage branches.
func (s *Scheduler) SuspendChildParent(ctx context.Context, runID string) (*ChildParentSuspension, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	lease := &ChildParentSuspension{scheduler: s, runID: runID}
	if admitted, ok := s.admittedRuns[runID]; ok {
		if admitted.owners != 1 || admitted.retained || admitted.executionWorkflow != "" {
			return nil, errors.New("localscheduler: child wait requires one exclusive parent owner")
		}
		lease.generation = admitted.generation
		if !admitted.suspended {
			admitted.suspended = true
			s.admittedRuns[runID] = admitted
			s.conditions.ReleaseWorkflow(admitted.identity)
		}
		return lease, nil
	}
	if reconciled, ok := s.reconciledRuns[runID]; ok {
		if reconciled.identity.Workflow != reconciled.workflow {
			return nil, errors.New("localscheduler: child cannot suspend parent capacity")
		}
		lease.reconciled = true
		if !reconciled.suspended {
			reconciled.suspended = true
			s.reconciledRuns[runID] = reconciled
			s.conditions.ReleaseWorkflow(reconciled.identity)
		}
		return lease, nil
	}
	return nil, errors.New("localscheduler: parent has no admission owner")
}

// Resume tries to reacquire concurrency for the same execution. Capacity refusal
// leaves the parent parked; callers retry with cancellation-aware backoff. It is
// idempotent, does not consume a new start budget, and cannot resurrect a released
// or superseded admission after parent cancellation.
func (p *ChildParentSuspension) Resume(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := p.scheduler
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.reconciled {
		return p.resumeReconciled()
	}
	a, ok := s.admittedRuns[p.runID]
	if !ok || a.generation != p.generation {
		return errors.New("localscheduler: parent admission ended")
	}
	if !a.suspended {
		return nil
	}
	if err := s.resumeChildParent(a.identity); err != nil {
		return err
	}
	a.suspended = false
	s.admittedRuns[p.runID] = a
	return nil
}

func (p *ChildParentSuspension) resumeReconciled() error {
	s := p.scheduler
	a, ok := s.reconciledRuns[p.runID]
	if !ok {
		return errors.New("localscheduler: parent admission ended")
	}
	if !a.suspended {
		return nil
	}
	if err := s.resumeChildParent(a.identity); err != nil {
		return err
	}
	a.suspended = false
	s.reconciledRuns[p.runID] = a
	return nil
}

// Both scheduler locks are held by the caller. Reuse continuation policy: a
// resumed run already consumed its start budget; current disabled/placement
// policy still forbids returning to agent execution.
func (s *Scheduler) resumeChildParent(identity WorkflowIdentity) error {
	entry, ok := s.workflows[identity]
	if !ok {
		return errors.New("localscheduler: parent workflow unavailable")
	}
	if reason, refused := s.permanentDispatchRefusal(entry); refused {
		return &TriggerRejectedError{Workflow: identity.Workflow, Reason: reason}
	}
	if ok, reason := s.conditions.ReserveContinuation(identity, entry.Readiness); !ok {
		return &TriggerRejectedError{Workflow: identity.Workflow, Reason: reason}
	}
	return nil
}
