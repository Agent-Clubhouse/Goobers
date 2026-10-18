package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildCancelledEscalationSettlesInvocationWithoutRewritingHistory(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	dir, err := f.launcher.layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := journal.TryRecover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	f.launcher.result = f.launcher.captureTerminal
	f.service.observeChild = acceptedChildObserver(f.launcher.layout)
	joined := true
	f.launcher.reconcile = func(context.Context, *journal.Reader) error {
		if !joined {
			return invoke.ErrWorkspaceNotQuiescent
		}
		return nil
	}
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	child, _ := f.state(t)
	if child.State != triggerqueue.ChildAwaitingHuman || child.ResultRef != "" {
		t.Fatal("escalation prematurely settled", child)
	}
	cancelledAt := time.Now()
	if err := f.service.queue.FenceChildParent(t.Context(), child.Identity.ChildParent, "operator", cancelledAt); err != nil {
		t.Fatal(err)
	}
	f.authority.revoked.Store(true)
	joined = false
	_, receipt := f.state(t)
	ref, err := f.service.childReference(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.launcher.Result(t.Context(), ref); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("unjoined escalation settled", err)
	}
	child, _ = f.state(t)
	if child.State != triggerqueue.ChildAwaitingHuman {
		t.Fatal("missing stop proof erased wait", child)
	}
	joined = true
	for range 2 {
		if err := f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	child, _ = f.state(t)
	if child.State != triggerqueue.ChildCancelled || child.ResultRef == "" || !child.AcknowledgedAt.IsZero() || f.executor.calls.Load() != 0 {
		t.Fatal("cancelled escalation did not retain result and slot", child)
	}
	after, err := rd.Events()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("cancellation rewrote terminal execution history", err)
	}
	pending, err := f.service.queue.PendingChildCancellations(t.Context(), child.Identity.ChildParent, "", 100)
	if err != nil || len(pending) != 0 {
		t.Fatal("cancelled escalation stayed in outbox", pending, err)
	}
	if err := f.service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := triggerqueue.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	coordinator := childworkflow.WorkspaceCoordinator{Queue: reopened}
	result, err := coordinator.ReadResult(t.Context(), child, "")
	if err != nil || result.Input.State != triggerqueue.ChildCancelled || !result.Input.FinishedAt.Equal(cancelledAt) {
		t.Fatal("reopened cancellation receipt changed", result, err)
	}
}
