package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildDrainFinishesCancelledOrphanOnlyAfterWorkerReconciliation(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	f.launcher.result = f.launcher.captureTerminal
	f.service.observeChild = acceptedChildObserver(f.launcher.layout)
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Revocation is intentional: cancellation cleanup must not need authority
	// to execute another stage or mint a credential.
	f.authority.revoked.Store(true)
	joined := false
	f.launcher.reconcile = func(context.Context, *journal.Reader) error {
		if !joined {
			return invoke.ErrWorkspaceNotQuiescent
		}
		return nil
	}
	_, receipt := f.state(t)
	ref, err := f.service.childReference(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.launcher.Result(t.Context(), ref); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("unjoined worker terminalized", err)
	}
	dir, err := f.launcher.layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if phase, err := reader.Phase(); err != nil || phase != journal.PhaseRunning {
		t.Fatal("unjoined worker claimed stopped", phase, err)
	}
	joined = true
	for range 2 {
		if err := f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	child, receipt := f.state(t)
	if child.State != triggerqueue.ChildCancelled || child.ResultRef == "" || receipt.State != triggerqueue.Dispatched || f.executor.calls.Load() != 0 {
		t.Fatal("cancelled orphan failed to converge without execution", child, receipt, f.executor.calls.Load())
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	finishes := 0
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			finishes++
			if event.Status != string(journal.PhaseAborted) {
				t.Fatal("cancelled child recorded another terminal phase", event.Status)
			}
		}
	}
	if finishes != 1 {
		t.Fatal("cancellation replay duplicated terminalization", finishes)
	}
}
