package main

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestRecoveredParentObservesAcceptanceWithoutReauthorizing(t *testing.T) {
	f := newHandoffDaemonFixture(t)
	recovered, err := f.host.Recover(t.Context(), f.env)
	if err != nil || recovered.Action != "wait" || recovered.ChildRunID != f.child.RunID || recovered.Origin != *f.env.ChildWorkflowOrigin {
		t.Fatal(recovered, err)
	}
	// Revoking the invocation grant does not erase its already accepted receipt.
	if err = f.queue.RevokeChildAuthority(t.Context(), f.grant); err != nil {
		t.Fatal(err)
	}
	after, err := f.host.Recover(t.Context(), f.env)
	if err != nil || after != recovered {
		t.Fatal("accepted wait lost with grant", after, err)
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: f.queue}
	result, err := coordinator.CaptureResult(t.Context(), f.child, nil, childworkflow.TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: time.Now(), Summary: "child stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.queue.SetChildState(t.Context(), f.child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: result.ResultRef}, time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err = f.host.Recover(t.Context(), f.env)
	if err != nil || after != recovered {
		t.Fatal("terminal result skipped original parent", after, err)
	}
	if err = f.queue.FenceChildParent(t.Context(), f.child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err = f.host.Recover(t.Context(), f.env)
	if !errors.Is(err, triggerqueue.ErrParentCancelled) || after.RequestID != "" {
		t.Fatal("cancelled family restored runnable wait", after, err)
	}
}
