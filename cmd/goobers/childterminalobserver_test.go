package main

import (
	"crypto/sha256"
	"fmt"
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

func TestChildTerminalObserverCapturesControlledPrestartDisposition(t *testing.T) {
	for _, disposition := range []string{"cancelled", "expired"} {
		t.Run(disposition, func(t *testing.T) {
			f := actualChildLaunchFixture(t)
			f.launcher.result = f.launcher.captureTerminal
			child, receipt := f.state(t)
			at := receipt.AcceptedAt.Add(time.Minute)
			scope := triggerqueue.StartScope{Gaggle: child.Identity.Gaggle, Workflow: f.submission.Envelope.Workflow, Kind: childworkflow.ChildStartKind, Source: "child", Generation: f.submission.Envelope.ConfigGeneration, PayloadDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(receipt.Payload)), ReservedRunID: child.RunID, Deadline: at}
			if _, err := f.service.queue.PinStartControl(t.Context(), receipt.ID, scope); err != nil {
				t.Fatal(err)
			}
			var err error
			if disposition == "cancelled" {
				_, _, err = f.service.queue.RequestStartCancellation(t.Context(), scope.Gaggle, receipt.ID, triggerqueue.StartCancellation{RequestID: "cancel", Actor: "operator", Reason: "No longer required", Authority: []byte(`{"issuer":"trusted","subject":"operator"}`)}, at)
			} else {
				_, _, err = f.service.queue.ExpireStartControl(t.Context(), scope.Gaggle, receipt.ID, at)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.service.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			child, receipt = f.state(t)
			expected := triggerqueue.ChildCancelled
			if disposition == "expired" {
				expected = triggerqueue.ChildFailed
			}
			if child.State != expected || receipt.State != triggerqueue.Rejected || receipt.RunID != "" || f.executor.calls.Load() != 0 || !child.AcknowledgedAt.IsZero() {
				t.Fatalf("controlled prestart disposition=%+v receipt=%+v", child, receipt)
			}
			coordinator := childworkflow.WorkspaceCoordinator{Queue: f.service.queue}
			result, err := coordinator.ReadResult(t.Context(), child, "")
			if err != nil || result.Input.State != expected || !result.Input.FinishedAt.Equal(at) {
				t.Fatalf("lost durable disposition: %+v %v", result, err)
			}
		})
	}
}
