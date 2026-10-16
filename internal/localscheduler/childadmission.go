package localscheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// reconciledRun separates execution display identity from the budget owner.
// Ordinary runs use the same workflow for both. A generated child charges its
// immutable parent's bucket even if its display name collides with the catalog.
type reconciledRun struct {
	identity  WorkflowIdentity
	workflow  string
	suspended bool
}

func reconciledRunFor(id journal.RunIdentity) reconciledRun {
	identity := WorkflowIdentity{Gaggle: id.Gaggle, Workflow: id.Workflow}
	if id.Child != nil {
		identity.Workflow = id.Child.ParentWorkflow
	}
	return reconciledRun{identity: identity, workflow: id.Workflow}
}
func (a runAdmission) workflowName() string {
	if a.executionWorkflow != "" {
		return a.executionWorkflow
	}
	return a.identity.Workflow
}

// ChildAdmissionRequest is trusted prepared execution, not authored proposal
// data. The caller has already checked queue custody, the parent generation,
// current policy and workspace readiness. Child is ephemeral and never added to
// the scheduler catalog; its own Readiness cannot widen the parent's limits.
type ChildAdmissionRequest struct {
	RunID       string
	Parent      WorkflowIdentity
	ParentRunID string
	Child       WorkflowEntry
}

// ReserveChild acquires normal provider, resource, concurrency and budget
// admission for a generated run, charged to its configured parent workflow.
// It NEVER releases or transfers a parent's permit. A max-concurrency=1 parent
// must first durably suspend every runnable owner through the wait coordinator.
// The returned release is idempotent and cannot release another run's permit.
// Starting, journal publication and async execution remain the caller's job.
func (s *Scheduler) ReserveChild(ctx context.Context, request ChildAdmissionRequest, now time.Time) (func(), error) {
	noop := func() {}
	if err := ctx.Err(); err != nil {
		return noop, err
	}
	if !validChildAdmissionRequest(request, now) {
		return noop, errors.New("localscheduler: invalid prepared child admission")
	}
	// This is a production durability boundary, unlike ordinary low-level test
	// dispatch's optional logging. Missing history must never reset family budget.
	if s.log == nil {
		return noop, errors.New("localscheduler: child admission requires durable instance journal")
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	parent, err := s.childParentEntry(request)
	if err != nil {
		return noop, err
	}
	if err = s.childAdmissionRefusal(parent, request.Child, now); err != nil {
		return noop, err
	}
	ok, reason := s.conditions.admitProviderWorkflow(request.Parent, quotaProvider(parent.RepoRef.Provider), parent.Readiness, now, false)
	if !ok {
		return noop, &TriggerRejectedError{Workflow: request.Child.Workflow, Reason: reason}
	}
	// Each successful reservation is an admission attempt, including a crash
	// before run publication. A later retry must not erase that budget charge.
	if err = s.recordChildAdmission(request, now); err != nil {
		s.conditions.ReleaseWorkflow(request.Parent)
		return noop, err
	}
	s.mu.Lock()
	s.nextAdmissionGeneration++
	generation := s.nextAdmissionGeneration
	s.admittedRuns[request.RunID] = runAdmission{identity: request.Parent, executionWorkflow: request.Child.Workflow, generation: generation, owners: 1}
	s.mu.Unlock()
	s.recordGaggleDispatch(request.Parent.Gaggle)
	return s.admissionOwnerRelease(request.RunID, request.Child.Workflow, generation), nil
}

func (s *Scheduler) childParentEntry(request ChildAdmissionRequest) (WorkflowEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.workflows[request.Parent]
	if !ok {
		return WorkflowEntry{}, errors.New("localscheduler: child parent workflow unavailable")
	}
	if _, ok = s.admittedRuns[request.RunID]; ok {
		return WorkflowEntry{}, errors.New("localscheduler: child run already admitted")
	}
	if _, ok = s.reconciledRuns[request.RunID]; ok {
		return WorkflowEntry{}, errors.New("localscheduler: child run already reconciled")
	}
	if parent.RepoRef.Provider != request.Child.RepoRef.Provider {
		return WorkflowEntry{}, errors.New("localscheduler: child provider differs from parent admission")
	}
	return parent, nil
}

func (s *Scheduler) childAdmissionRefusal(parent, child WorkflowEntry, now time.Time) error {
	if retryAt, open := s.authCircuitRetryAt(entryIdentity(parent), now); open {
		return &TriggerRejectedError{Workflow: child.Workflow, Reason: authCircuitReason(retryAt)}
	}
	for _, entry := range []WorkflowEntry{parent, child} {
		if reason, refused := s.permanentDispatchRefusal(entry); refused {
			return &TriggerRejectedError{Workflow: child.Workflow, Reason: reason}
		}
	}
	return nil
}

func (s *Scheduler) recordChildAdmission(request ChildAdmissionRequest, now time.Time) error {
	attempt, err := dispatchRunID("")
	if err != nil {
		return err
	}
	return s.appendJournalEvent(journal.Event{Type: journal.EventRunStarted, Workflow: request.Parent.Workflow, Gaggle: request.Parent.Gaggle, RunID: request.RunID, Time: now, Reason: "generated child admitted", Runner: map[string]any{
		"note": "child.admission", "childWorkflow": request.Child.Workflow, "parentRunId": request.ParentRunID, "childAdmissionId": attempt,
	}})
}

func childAdmissionBudgetKey(event journal.Event) (string, bool) {
	if event.Runner["note"] != "child.admission" {
		return "", false
	}
	id, ok := event.Runner["childAdmissionId"].(string)
	return id, ok && apiv1.ValidRunID(id) && event.Gaggle != "" && event.Workflow != "" && apiv1.ValidRunID(event.RunID)
}

// childBudgetOwners binds driver echoes to the original bucket, including when
// the generated display name is also a configured workflow in this gaggle.
func childBudgetOwners(events []journal.Event) map[string]WorkflowIdentity {
	owners := map[string]WorkflowIdentity{}
	for _, event := range events {
		if _, ok := childAdmissionBudgetKey(event); ok {
			owners[fmt.Sprintf("%s/%s", event.Gaggle, event.RunID)] = WorkflowIdentity{Gaggle: event.Gaggle, Workflow: event.Workflow}
		}
	}
	return owners
}

func validChildAdmissionRequest(request ChildAdmissionRequest, now time.Time) bool {
	return apiv1.ValidRunID(request.RunID) && apiv1.ValidRunID(request.ParentRunID) && request.RunID != request.ParentRunID && request.Parent.Workflow != "" && request.Parent.Gaggle != "" && request.Child.Gaggle == request.Parent.Gaggle && request.Child.Workflow != "" && !now.IsZero()
}
