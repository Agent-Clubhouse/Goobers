package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// #3651: a close that never returns used to hang `goobers up` forever because
// shutdown ran on an unbounded background context. Shutdown must give up and
// name the step it was waiting on.
func TestSchedulerSetupShutdownBoundsWedgedStep(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	restore := schedulerShutdownGrace
	schedulerShutdownGrace = 50 * time.Millisecond
	t.Cleanup(func() { schedulerShutdownGrace = restore })

	setup := &schedulerSetup{StopProjector: func() { <-release }}
	done := make(chan error, 1)
	go func() { done <- setup.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Shutdown returned nil for a wedged step")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error = %v, want context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "read model projector") {
			t.Fatalf("Shutdown error %q does not name the wedged step", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return: shutdown is unbounded")
	}
}

// #3651: shutdown must not report clean completion after losing final
// persisted state — every step's failure is joined into the returned error.
func TestRunShutdownStepsJoinsFailures(t *testing.T) {
	flushErr := errors.New("flush wedged")
	closeErr := errors.New("db locked")
	var order []string
	err := runShutdownSteps(context.Background(), []shutdownStep{
		{"telemetry client", func() error { order = append(order, "telemetry client"); return flushErr }},
		{"read model store", func() error { order = append(order, "read model store"); return nil }},
		{"instance log", func() error { order = append(order, "instance log"); return closeErr }},
	})
	if err == nil {
		t.Fatal("runShutdownSteps returned nil despite failing steps")
	}
	if !errors.Is(err, flushErr) || !errors.Is(err, closeErr) {
		t.Fatalf("runShutdownSteps error = %v, want both step errors joined", err)
	}
	for _, want := range []string{"shut down telemetry client", "shut down instance log"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runShutdownSteps error %q missing %q", err, want)
		}
	}
	if strings.Join(order, ",") != "telemetry client,read model store,instance log" {
		t.Fatalf("steps ran out of order: %v", order)
	}
}

// A successful shutdown still reports success, and closing twice (explicit
// call plus the caller's safety-net defer) must not close anything twice.
func TestSchedulerSetupShutdownSucceedsOnceAndIsRepeatable(t *testing.T) {
	var stops atomic.Int64
	setup := &schedulerSetup{StopProjector: func() { stops.Add(1) }}
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("StopProjector called %d times, want 1", got)
	}
}

// Nil-safety (#129): a caller defers Shutdown before knowing whether telemetry
// or the read model were ever constructed.
func TestSchedulerSetupShutdownNilSafe(t *testing.T) {
	var setup *schedulerSetup
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil setup Shutdown: %v", err)
	}
	if err := (&schedulerSetup{}).Shutdown(context.Background()); err != nil {
		t.Fatalf("empty setup Shutdown: %v", err)
	}
}
