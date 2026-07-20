package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
)

func TestClaimLockUncontendedIsLowNoise(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	lockPath := filepath.Join(schedulerDir, claimLockFileName)
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := withClaimLockThreshold(lockPath, "test.uncontended", time.Second, func() error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("uncontended lock emitted %d events, want none: %+v", len(events), events)
	}
}

func TestClaimLockDelayedHolderReportsSlowWaitAndOperation(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	lockPath := filepath.Join(schedulerDir, claimLockFileName)
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	holder, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	const (
		threshold = 10 * time.Millisecond
		holdFor   = 30 * time.Millisecond
		operation = claimLockOperationBacklogClaim
	)
	released := make(chan struct{})
	go func() {
		time.Sleep(holdFor)
		_ = holder.Release()
		close(released)
	}()

	if err := withClaimLockThreshold(lockPath, operation, threshold, func() error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-released

	event := readSingleSlowClaimLockEvent(t, schedulerDir, operation)
	waitDuration := claimLockEventDuration(t, event, "waitDuration")
	if waitDuration <= threshold {
		t.Fatalf("waitDuration = %s, want above %s", waitDuration, threshold)
	}
	_ = claimLockEventDuration(t, event, "holdDuration")
}

func TestClaimLockSlowHoldReportsBothDurations(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	lockPath := filepath.Join(schedulerDir, claimLockFileName)
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		threshold = 10 * time.Millisecond
		operation = claimLockOperationRunRelease
	)
	if err := withClaimLockThreshold(lockPath, operation, threshold, func() error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	event := readSingleSlowClaimLockEvent(t, schedulerDir, operation)
	_ = claimLockEventDuration(t, event, "waitDuration")
	holdDuration := claimLockEventDuration(t, event, "holdDuration")
	if holdDuration <= threshold {
		t.Fatalf("holdDuration = %s, want above %s", holdDuration, threshold)
	}
}

func readSingleSlowClaimLockEvent(t *testing.T, schedulerDir, operation string) journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d instance events, want one: %+v", len(events), events)
	}
	event := events[0]
	if event.Type != journal.EventClaimLockSlow {
		t.Fatalf("event type = %q, want %q", event.Type, journal.EventClaimLockSlow)
	}
	if got := event.Runner["operation"]; got != operation {
		t.Fatalf("operation = %v, want %q", got, operation)
	}
	if got := event.Runner["pid"]; got != float64(os.Getpid()) {
		t.Fatalf("pid = %v, want %d", got, os.Getpid())
	}
	return event
}

func claimLockEventDuration(t *testing.T, event journal.Event, field string) time.Duration {
	t.Helper()
	value, ok := event.Runner[field].(string)
	if !ok {
		t.Fatalf("%s = %#v, want duration string", field, event.Runner[field])
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("parse %s %q: %v", field, value, err)
	}
	return duration
}
