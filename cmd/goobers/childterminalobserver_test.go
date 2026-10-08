package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildTerminalObserverRetainsRealCompletionAcrossQueueReopen(t *testing.T) {
	f := actualChildLaunchFixture(t)
	f.launcher.result = f.launcher.captureTerminal
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	for range 2 {
		if err := f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	child, receipt := f.state(t)
	if child.State != triggerqueue.ChildCompleted || receipt.State != triggerqueue.Dispatched || child.ResultRef == "" || child.WorkspaceRef != "" {
		t.Fatalf("terminal custody=%+v %s", child, receipt.State)
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: f.service.queue}
	result, err := coordinator.ReadResult(t.Context(), child, "")
	if err != nil || result.Input.FinishedAt.IsZero() {
		t.Fatalf("retained result=%+v %v", result, err)
	}
	if err = f.service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = acceptedService(t, f.path, newDaemonTriggerService())
	coordinator.Queue = f.service.queue
	after, err := coordinator.ReadResult(t.Context(), child, "")
	if err != nil || after.ResultRef != result.ResultRef || !after.Input.FinishedAt.Equal(result.Input.FinishedAt) {
		t.Fatal("retry changed terminal receipt")
	}
	if child.AcknowledgedAt.IsZero() == false {
		t.Fatal("observation acknowledged parent slot")
	}
}

func TestChildTerminalObserverUsesDurablePrestartRejection(t *testing.T) {
	f := actualChildLaunchFixture(t)
	f.launcher.result = f.launcher.captureTerminal
	at := time.Now()
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", at); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	child, receipt := f.state(t)
	if child.State != triggerqueue.ChildCancelled || receipt.State != triggerqueue.Rejected || f.executor.calls.Load() != 0 {
		t.Fatalf("rejection custody: %s %s", child.State, receipt.State)
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: f.service.queue}
	result, err := coordinator.ReadResult(t.Context(), child, "")
	if err != nil || !result.Input.FinishedAt.Equal(at) {
		t.Fatalf("rejection time invented: %+v %v", result, err)
	}
}

func TestChildCancelledUncertainRecoveryCannotBecomeAcceptedAgain(t *testing.T) {
	f := newChildDrainFixture(t)
	if err := f.service.queue.BeginDispatch(t.Context(), f.submission.Child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.queue.MarkChildParentSettled(t.Context(), f.submission.Child.Identity.ChildParent, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, receipt := f.state(t)
	if receipt.State != triggerqueue.Rejected || f.launcher.starts != 0 {
		t.Fatalf("cancelled recovery=%s starts=%d", receipt.State, f.launcher.starts)
	}
}
