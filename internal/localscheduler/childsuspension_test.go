package localscheduler

import (
	"testing"
	"time"
)

func TestChildWaitSuspendsConcurrencyPreservingOwnerAndBudget(t *testing.T) {
	parent, request := childAdmissionFixture()
	s, _ := newTestScheduler(t, []WorkflowEntry{parent})
	cleanup, ok, reason := s.ReserveContinuation(request.ParentRunID, parent.Gaggle, parent.Workflow)
	if !ok {
		t.Fatal(reason)
	}
	wait, err := s.SuspendChildParent(t.Context(), request.ParentRunID)
	if err != nil {
		t.Fatal(err)
	}
	childRelease, err := s.ReserveChild(t.Context(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = wait.Resume(t.Context()); err == nil {
		t.Fatal("parent resumed over child capacity")
	}
	if _, ok, _ := s.ReserveContinuation(request.ParentRunID, parent.Gaggle, parent.Workflow); ok {
		t.Fatal("intervention bypassed parked parent")
	}
	childRelease()
	if err = wait.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = wait.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(s.conditions.starts[request.Parent]); got != 1 {
		t.Fatalf("resume charged extra start: %d", got)
	}
	if got := s.conditions.ActiveWorkflow(request.Parent); got != 1 {
		t.Fatalf("parent capacity=%d", got)
	}
	cleanup()
	if got := s.conditions.ActiveWorkflow(request.Parent); got != 0 {
		t.Fatalf("original cleanup leaked resumed owner=%d", got)
	}
}

func TestChildWaitCancelledParentCannotReleaseChildOrResurrect(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "watchdog"}[force], func(t *testing.T) {
			parent, request := childAdmissionFixture()
			s, _ := newTestScheduler(t, []WorkflowEntry{parent})
			cleanup, _, _ := s.ReserveContinuation(request.ParentRunID, parent.Gaggle, parent.Workflow)
			wait, err := s.SuspendChildParent(t.Context(), request.ParentRunID)
			if err != nil {
				t.Fatal(err)
			}
			release, err := s.ReserveChild(t.Context(), request, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if force {
				s.ReleaseRun(request.ParentRunID, parent.Workflow)
			} else {
				cleanup()
			}
			if got := s.conditions.ActiveWorkflow(request.Parent); got != 1 {
				t.Fatalf("parent cleanup stole child permit: %d", got)
			}
			if err = wait.Resume(t.Context()); err == nil {
				t.Fatal("resurrected parent admission")
			}
			release()
			cleanup()
			if got := s.conditions.ActiveWorkflow(request.Parent); got != 0 {
				t.Fatal(got)
			}
		})
	}
}

func TestChildWaitRefusesMultipleOwnersAndDisabledResume(t *testing.T) {
	parent, request := childAdmissionFixture()
	s, _ := newTestScheduler(t, []WorkflowEntry{parent})
	cleanup, _, _ := s.ReserveContinuation(request.ParentRunID, parent.Gaggle, parent.Workflow)
	defer cleanup()
	sibling, _, _ := s.ReserveContinuation(request.ParentRunID, parent.Gaggle, parent.Workflow)
	if _, err := s.SuspendChildParent(t.Context(), request.ParentRunID); err == nil {
		t.Fatal("suspended multiple runnable owners")
	}
	sibling()
	wait, err := s.SuspendChildParent(t.Context(), request.ParentRunID)
	if err != nil {
		t.Fatal(err)
	}
	parent.DisabledReason = "operator disabled"
	s.mu.Lock()
	s.workflows[request.Parent] = parent
	s.mu.Unlock()
	if err = wait.Resume(t.Context()); err == nil {
		t.Fatal("resumed revoked parent")
	}
}

func TestChildWaitReconciledOwnershipCleanup(t *testing.T) {
	parent, request := childAdmissionFixture()
	s, _ := newTestScheduler(t, []WorkflowEntry{parent})
	s.reconciledRuns[request.ParentRunID] = reconciledRun{identity: request.Parent, workflow: parent.Workflow}
	if ok, _ := s.conditions.ReserveContinuation(request.Parent, parent.Readiness); !ok {
		t.Fatal("fixture capacity")
	}
	wait, err := s.SuspendChildParent(t.Context(), request.ParentRunID)
	if err != nil {
		t.Fatal(err)
	}
	childRelease, err := s.ReserveChild(t.Context(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.ReleaseReconciled(request.ParentRunID, parent.Workflow)
	if got := s.conditions.ActiveWorkflow(request.Parent); got != 1 {
		t.Fatalf("recovery cleanup stole child permit: %d", got)
	}
	if err = wait.Resume(t.Context()); err == nil {
		t.Fatal("resurrected recovered parent")
	}
	childRelease()
}
