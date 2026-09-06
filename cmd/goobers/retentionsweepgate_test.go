package main

import (
	"errors"
	"testing"
	"time"
)

// TestRetentionSweepGateCoalescesConcurrentRuns is #4373's acceptance
// criteria for "Only one retention sweep may run at a time" and "Concurrent
// startup/periodic/manual sweep requests coalesce or return a typed
// already-running result": a second caller while one sweep is in flight
// gets errRetentionSweepAlreadyRunning immediately rather than blocking
// behind it or running concurrently.
func TestRetentionSweepGateCoalescesConcurrentRuns(t *testing.T) {
	gate := &retentionSweepGate{}
	inFlight := make(chan struct{})
	release := make(chan struct{})

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- gate.run(func() error {
			close(inFlight)
			<-release
			return nil
		})
	}()

	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("first sweep never started")
	}

	if err := gate.run(func() error {
		t.Fatal("second sweep ran concurrently with the first")
		return nil
	}); !errors.Is(err, errRetentionSweepAlreadyRunning) {
		t.Fatalf("concurrent gate.run = %v, want errRetentionSweepAlreadyRunning", err)
	}

	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first sweep returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first sweep never returned")
	}

	// Once the first sweep has finished, the gate must allow another run
	// (not permanently wedge itself into "always running").
	ran := false
	if err := gate.run(func() error { ran = true; return nil }); err != nil {
		t.Fatalf("gate.run after release: %v", err)
	}
	if !ran {
		t.Fatal("gate did not run the sweep after the prior one released")
	}
}
