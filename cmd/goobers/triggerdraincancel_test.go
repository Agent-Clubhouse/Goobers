package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDurableTriggerCancelledWaitDoesNotInterruptActiveDrain(t *testing.T) {
	dispatch := newDaemonTriggerService()
	barrier := &barrierTriggerer{entered: make(chan struct{}), release: make(chan struct{})}
	dispatch.dispatch = barrier
	service := acceptedService(t, filepath.Join(t.TempDir(), "queue.db"), dispatch)
	accepted, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "worker", RequestID: "first", Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	active := make(chan error, 1)
	go func() { active <- service.Drain(t.Context()) }()
	var once sync.Once
	release := func() { once.Do(func() { close(barrier.release) }) }
	defer release()
	select {
	case <-barrier.entered:
	case err := <-active:
		t.Fatalf("first drain exited before dispatch: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first drain did not reach dispatch")
	}
	waitingCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting := make(chan error, 1)
	started := make(chan struct{})
	go func() { close(started); waiting <- service.Drain(waitingCtx) }()
	<-started
	cancel()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter returned %v", err)
		}
	case <-time.After(5 * time.Second):
		release()
		<-active
		<-waiting
		t.Fatal("cancelled caller remained blocked behind active drain")
	}
	select {
	case err := <-active:
		t.Fatalf("cancelling another caller interrupted active drain: %v", err)
	default:
	}
	record, err := service.queue.Get(t.Context(), accepted.AcceptanceID, "operator")
	if err != nil || record.State != triggerqueue.Dispatching || barrier.mints.Load() != 1 {
		t.Fatal(record, err, barrier.mints.Load())
	}
	release()
	if err := <-active; err != nil {
		t.Fatal(err)
	}
	// A cancelled waiter must not retain the guard or damage the active owner's
	// custody. A subsequent pass must still admit a different accepted start.
	if _, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "worker", RequestID: "second", Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if barrier.mints.Load() != 2 {
		t.Fatalf("subsequent start count = %d", barrier.mints.Load())
	}
}

func TestDurableTriggerAlreadyCancelledDrainDoesNotTouchQueue(t *testing.T) {
	// No queue or dispatcher is needed: an already-cancelled pass must stop
	// before retention, reconciliation, or cursor reservation can begin.
	service := &durableTriggerService{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := service.Drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
