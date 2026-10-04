package localscheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func parkedParentJournal(t *testing.T, root string, parent WorkflowEntry, request ChildAdmissionRequest, forged bool) {
	t.Helper()
	run, err := journal.Create(root, journal.RunIdentity{RunID: request.ParentRunID, Gaggle: parent.Gaggle, Workflow: parent.Workflow}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	_, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if forged {
		origin.StageOccurrence = "foreign"
	}
	receipt := runner.ChildHandoffRequest{Gaggle: parent.Gaggle, ParentRunID: request.ParentRunID, RequestID: journal.Digest([]byte("wait")), Action: "wait", ChildRunID: request.RunID, AcceptanceID: "trigger-" + request.RunID, InvocationKey: "child", SourceDigest: journal.Digest([]byte("source")), Origin: *origin}
	if err = run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": runner.ChildWaitKind, "childWait": map[string]any{"version": 1, "parentRunId": request.ParentRunID, "request": receipt}}}); err != nil {
		t.Fatal(err)
	}
}

func TestChildWaitRestartPreservesParentOwnerWithoutChargingItsCapacity(t *testing.T) {
	for _, inventory := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "inventory"}[inventory], func(t *testing.T) {
			parent, request := childAdmissionFixture()
			root := t.TempDir()
			parkedParentJournal(t, root, parent, request, false)
			child, err := journal.Create(root, journal.RunIdentity{RunID: request.RunID, Gaggle: parent.Gaggle, Workflow: request.Child.Workflow, ConfigGeneration: journal.Digest([]byte("generation")), WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers")),
				Child: &journal.ChildLineage{Gaggle: parent.Gaggle, ParentRunID: request.ParentRunID, ParentWorkflow: parent.Workflow, StageOccurrence: "plan", InvocationKey: "child", AcceptanceID: "trigger-" + request.RunID, SourceDigest: journal.Digest([]byte("source")), EnvelopeDigest: journal.Digest([]byte("envelope"))}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = child.Close(); err != nil {
				t.Fatal(err)
			}
			s, _ := newTestScheduler(t, []WorkflowEntry{parent})
			if inventory {
				err = s.ReconcileRunDirs([]string{root}, []string{filepath.Join(root, request.ParentRunID), filepath.Join(root, request.RunID)}, time.Now())
			} else {
				err = s.Reconcile(root, time.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := s.conditions.ActiveWorkflow(request.Parent); got != 1 {
				t.Fatalf("parked parent still holds slot: %d", got)
			}
			if !s.reconciledRuns[request.ParentRunID].suspended {
				t.Fatal("lost durable suspended owner")
			}
			wait, err := s.SuspendChildParent(t.Context(), request.ParentRunID)
			if err != nil {
				t.Fatal(err)
			}
			if err = wait.Resume(t.Context()); err == nil {
				t.Fatal("parent resumed while child holds sole slot")
			}
			s.ReleaseReconciled(request.RunID, request.Child.Workflow)
			if err = wait.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			s.ReleaseReconciled(request.ParentRunID, parent.Workflow)
			if got := s.conditions.ActiveWorkflow(request.Parent); got != 0 {
				t.Fatalf("resumed ownership leak=%d", got)
			}
		})
	}
}

func TestChildWaitForgedRestartMarkerRetainsCapacity(t *testing.T) {
	parent, request := childAdmissionFixture()
	root := t.TempDir()
	parkedParentJournal(t, root, parent, request, true)
	s, _ := newTestScheduler(t, []WorkflowEntry{parent})
	if err := s.Reconcile(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := s.conditions.ActiveWorkflow(request.Parent); got != 1 {
		t.Fatalf("forged marker released capacity=%d", got)
	}
}
