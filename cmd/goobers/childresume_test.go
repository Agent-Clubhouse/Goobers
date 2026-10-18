package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildDrainResumesRecoveredNonterminalExactlyOnce(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	f.launcher.reconcile = func(context.Context, *journal.Reader) error { return nil }
	f.launcher.result = f.launcher.captureTerminal
	f.service.observeChild = acceptedChildObserver(f.launcher.layout)
	f.executor.entered, f.executor.proceed = make(chan struct{}), make(chan struct{})
	defer close(f.executor.proceed)
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.executor.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery never resumed")
	}
	for range 2 {
		if err := f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	child, receipt := f.state(t)
	if child.RunID != id.RunID || child.State != triggerqueue.ChildRunning || receipt.State != triggerqueue.Dispatched || f.executor.calls.Load() != 1 {
		t.Fatal(child, receipt, f.executor.calls.Load())
	}
	if f.authority.held.Load() != 0 {
		t.Fatal("authority held across recovery execution")
	}
}

func TestChildDrainRecoveryCannotResumeCancelledOrSettledFamily(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "settled"}[settled], func(t *testing.T) {
			f := actualChildLaunchFixture(t)
			publishInterruptedChild(t, f)
			f.launcher.reconcile = func(context.Context, *journal.Reader) error { return nil }
			f.launcher.result = f.launcher.captureTerminal
			_, receipt := f.state(t)
			ref, err := f.service.childReference(t.Context(), receipt)
			if err != nil {
				t.Fatal(err)
			}
			expected := triggerqueue.ErrParentCancelled
			if settled {
				expected = triggerqueue.ErrParentSettled
				err = f.service.queue.MarkChildParentSettled(t.Context(), ref.Child.Identity.ChildParent, time.Now())
			} else {
				err = f.service.queue.FenceChildParent(t.Context(), ref.Child.Identity.ChildParent, "operator", time.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.launcher.Result(t.Context(), ref)
			if settled {
				if !errors.Is(err, expected) {
					t.Fatal(err)
				}
			} else if err != nil || result.State != triggerqueue.ChildCancelled || result.ResultRef == "" {
				t.Fatal("cancelled recovery did not retain a terminal result", result, err)
			}
			f.wg.Wait()
			if f.executor.calls.Load() != 0 {
				t.Fatal("fenced recovery executed")
			}
		})
	}
}

func TestChildRecoveryOwnershipBarrierAllowsCancellationBeforeEffects(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	runtime, err := f.launcher.resolveGeneration(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runtime.runner.Resume(t.Context(), runner.ResumeInput{RunID: id.RunID, Machine: runtime.machine, GooberDigest: runtime.gooberDigest, OnRecoveryOwned: func(owned context.Context) error { close(ready); <-owned.Done(); return owned.Err() }})
		done <- err
	}()
	<-ready
	_, accepted, err := runtime.runner.CancelRun(id.RunID, time.Now())
	if err != nil || !accepted {
		t.Fatal("recovery not cancellable", accepted, err)
	}
	<-done
	if f.executor.calls.Load() != 0 {
		t.Fatal("cancelled recovery reached stage")
	}
}
