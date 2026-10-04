package localscheduler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
)

// This is an operation bucket, not a configured workflow. The reserved prefix
// cannot be a catalog workflow name. Human turns do not inherit automation's
// hourly cadence, but share its resource and instance concurrency conditions.
const sessionBudget = "@interactive-sessions"

func sessionAdmissionKey(id journal.RunIdentity) string {
	raw, _ := json.Marshal(struct {
		Gaggle, Workflow, Generation, WorkflowDigest, GooberDigest string
		Session                                                    *journal.SessionLineage
	}{id.Gaggle, id.Workflow, id.ConfigGeneration, id.WorkflowDigest, id.GooberDigest, id.Session})
	return journal.Digest(raw)
}

// ReserveSession admits a genuine accepted human turn without registering a
// fake workflow. The caller holds the current interactive execution lease and
// invokes release only after all writers have joined or no effects occurred.
// Generic watchdog terminalization cannot release this custody reservation.
func (s *Scheduler) ReserveSession(ctx context.Context, id journal.RunIdentity, now time.Time) (func(), error) {
	noop := func() {}
	if err := ctx.Err(); err != nil {
		return noop, err
	}
	if id.Session == nil || now.IsZero() || id.ValidateSessionLineage() != nil || s.log == nil {
		return noop, errors.New("localscheduler: session admission requires pinned provenance and durable audit")
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.sessionAlreadyOwned(id.RunID) {
		return noop, errors.New("localscheduler: session custody already owned")
	}
	bucket := WorkflowIdentity{Gaggle: id.Gaggle, Workflow: sessionBudget}
	ok, reason := s.conditions.admitProviderWorkflow(bucket, "", apiv1.ReadinessConditions{MaxConcurrentRuns: sessioning.MaxExecutingTurns}, now, true)
	if !ok {
		return noop, &TriggerRejectedError{Workflow: id.Workflow, Reason: reason}
	}
	if err := s.appendJournalEvent(journal.Event{Type: journal.EventRunnerAnnotation, RunID: id.RunID, Gaggle: id.Gaggle, Workflow: id.Workflow, Time: now, Runner: map[string]any{"kind": "session.admission", "sessionId": id.Session.SessionID, "turnId": id.Session.TurnID, "acceptanceId": id.Session.AcceptanceID, "inputDigest": id.Session.InputDigest}}); err != nil {
		s.conditions.ReleaseWorkflow(bucket)
		return noop, err
	}
	return s.installSessionOwner(id), nil
}

// RestoreSession restores previously admitted, still-unsettled execution
// custody, even when its run has a terminal event. It is called from verified
// queue+journal reconciliation before new dispatch, never from an HTTP request.
// It does not start a worker, recheck start budgets, or assert writer cessation.
func (s *Scheduler) RestoreSession(id journal.RunIdentity) (func(), error) {
	noop := func() {}
	if id.Session == nil || id.ValidateSessionLineage() != nil {
		return noop, errors.New("localscheduler: invalid session custody")
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	s.mu.Lock()
	_, owned := s.admittedRuns[id.RunID]
	restored, exists := s.reconciledRuns[id.RunID]
	if owned || (exists && restored.sessionKey != sessionAdmissionKey(id)) {
		s.mu.Unlock()
		return noop, errors.New("localscheduler: session custody already owned or mismatched")
	}
	if exists {
		delete(s.reconciledRuns, id.RunID)
	}
	s.mu.Unlock()
	if !exists {
		s.conditions.mu.Lock()
		s.conditions.active[WorkflowIdentity{Gaggle: id.Gaggle, Workflow: sessionBudget}]++
		s.conditions.totalActive++
		s.conditions.mu.Unlock()
	}
	return s.installSessionOwner(id), nil
}

func (s *Scheduler) sessionAlreadyOwned(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, admitted := s.admittedRuns[runID]
	_, restored := s.reconciledRuns[runID]
	return admitted || restored
}
func (s *Scheduler) installSessionOwner(id journal.RunIdentity) func() {
	s.mu.Lock()
	s.nextAdmissionGeneration++
	generation := s.nextAdmissionGeneration
	s.admittedRuns[id.RunID] = runAdmission{identity: WorkflowIdentity{Gaggle: id.Gaggle, Workflow: sessionBudget}, executionWorkflow: id.Workflow, generation: generation, owners: 1}
	s.mu.Unlock()
	return s.admissionOwnerRelease(id.RunID, id.Workflow, generation)
}

// Session execution echoes must not spend a catalog workflow's cadence when
// its display name happens to be interactive-session.
func bindSessionBudgetOwners(owners map[string]WorkflowIdentity, events []journal.Event) {
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == "session.admission" && apiv1.ValidRunID(event.RunID) && event.Gaggle != "" {
			owners[event.Gaggle+"/"+event.RunID] = WorkflowIdentity{Gaggle: event.Gaggle, Workflow: sessionBudget}
		}
	}
}
