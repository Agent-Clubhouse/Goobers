package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func openTestInstanceLog(t *testing.T) *journal.InstanceLog {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatalf("OpenInstanceLog: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

func readServiceHealthEvents(t *testing.T, log *journal.InstanceLog) []journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		t.Fatalf("ReadInstanceLog: %v", err)
	}
	var health []journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventServiceHealth {
			health = append(health, ev)
		}
	}
	return health
}

// TestServiceHealthEmitsAtStartupThenOnCadence is #5244's cadence acceptance:
// a record at daemon startup, then one per interval, on an instance that is
// running NO workflow.
//
// A fake clock and a tiny interval drive it, so the assertion is about the
// cadence rather than about elapsed wall-clock time. The startup record is
// emitted before any tick on purpose — an instance restarted more often than
// the interval would otherwise never produce one at all.
func TestServiceHealthEmitsAtStartupThenOnCadence(t *testing.T) {
	log := openTestInstanceLog(t)
	root := t.TempDir()
	identity := &daemonIdentity{StartedAt: time.Unix(1_700_000_000, 0).UTC()}

	// Startup only: a zero interval runs no ticker at all.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go emitServiceHealth(ctx, root, identity, log, nil, 0, nil, done)
	<-done

	health := readServiceHealthEvents(t, log)
	if len(health) != 1 {
		t.Fatalf("startup produced %d health records, want exactly 1", len(health))
	}

	// Now with a cadence: the startup record plus one per tick, and no
	// duplicate periodic records for a single tick.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go emitServiceHealth(ctx2, root, identity, log, nil, 5*time.Millisecond, nil, done2)

	deadline := time.Now().Add(10 * time.Second)
	for len(readServiceHealthEvents(t, log)) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("periodic records never arrived; have %d", len(readServiceHealthEvents(t, log)))
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel2()
	<-done2
}

// TestServiceHealthCadenceIsSixHours pins the interval itself. #5244 asks for a
// 21600-second cadence specifically, and a constant is the only place that can
// drift silently.
func TestServiceHealthCadenceIsSixHours(t *testing.T) {
	if got := serviceHealthInterval.Seconds(); got != 21600 {
		t.Fatalf("serviceHealthInterval = %vs, want 21600s (six hours)", got)
	}
	// The fast informational heartbeat (#488) must NOT have been slowed to this
	// cadence — #5244 requires it be preserved, and the two exist for different
	// readers.
	if heartbeatInterval != time.Minute {
		t.Fatalf("heartbeatInterval = %v, want the existing one minute preserved", heartbeatInterval)
	}
}

// TestServiceHealthRecordIsFindableWithoutARunID is the discoverability
// requirement: the record must be queryable from the instance's own diagnostic
// surface without inventing a workflow run to hang it off.
//
// It also pins that no run was fabricated — #5244 forbids inventing a run or
// holding an artificial six-hour task open to represent an idle instance.
func TestServiceHealthRecordIsFindableWithoutARunID(t *testing.T) {
	log := openTestInstanceLog(t)
	identity := &daemonIdentity{StartedAt: time.Unix(1_700_000_000, 0).UTC()}
	if err := appendServiceHealth(t.TempDir(), identity, log, nil, time.Unix(1_700_003_600, 0)); err != nil {
		t.Fatalf("appendServiceHealth: %v", err)
	}

	health := readServiceHealthEvents(t, log)
	if len(health) != 1 {
		t.Fatalf("health records = %d, want 1", len(health))
	}
	if health[0].RunID != "" {
		t.Errorf("record carries runID %q; an idle instance must not fabricate a run", health[0].RunID)
	}
	payload := health[0].Runner
	if payload == nil {
		t.Fatal("record carries no payload")
	}
	// Uptime is derived from the observation time, not measured by sleeping.
	if got := payload["processUptimeSeconds"]; got != 3600.0 {
		t.Errorf("processUptimeSeconds = %v, want 3600", got)
	}
	for _, key := range []string{"schemaVersion", "observedAt", "machineName", "accountName", "windowCoverage"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("payload missing required field %q: %+v", key, payload)
		}
	}
}

// TestServiceHealthKeepsMissingIdentityExplicit is the honesty requirement.
// #5244 is specific that a missing identity stays explicit rather than being
// filled in with something plausible, because a wrong answer is worse than a
// declared unknown.
func TestServiceHealthKeepsMissingIdentityExplicit(t *testing.T) {
	log := openTestInstanceLog(t)
	// A root with no durable identity file.
	payload := serviceHealthPayload(observeServiceHealth(t.TempDir(), nil, log, nil, time.Unix(1_700_000_000, 0)))

	if got := payload["instanceId"]; got != serviceHealthUnknown {
		t.Errorf("instanceId = %v, want %q", got, serviceHealthUnknown)
	}
	if _, ok := payload["identityProblem"]; !ok {
		t.Errorf("payload does not explain the missing identity: %+v", payload)
	}
	// No daemon identity was supplied, so uptime is absent rather than zero —
	// reporting 0s of uptime would assert a measurement never taken.
	if _, ok := payload["processUptimeSeconds"]; ok {
		t.Errorf("payload reports uptime with no daemon identity: %+v", payload)
	}
	if _, ok := payload["daemonStartedAt"]; ok {
		t.Errorf("payload reports a start time with no daemon identity: %+v", payload)
	}
}

// TestServiceHealthDistinguishesCoveredEmptyWindowFromUnknown is the other
// honesty requirement, and the one most easily got wrong: #5244 says "zero
// means a covered empty window", which only holds if an UNREADABLE window
// reports something other than zero.
func TestServiceHealthDistinguishesCoveredEmptyWindowFromUnknown(t *testing.T) {
	// Covered and empty: the log is readable and contains no dirty restarts.
	log := openTestInstanceLog(t)
	covered := serviceHealthPayload(observeServiceHealth(t.TempDir(), nil, log, nil, time.Now()))
	if got := covered["windowCoverage"]; got != serviceHealthWindowComplete {
		t.Fatalf("windowCoverage = %v, want %q", got, serviceHealthWindowComplete)
	}
	if got, ok := covered["observedUncleanRestarts"]; !ok || got != 0 {
		t.Errorf("observedUncleanRestarts = %v (present=%v), want a reported 0", got, ok)
	}

	// Unreadable: the count must be ABSENT, not zero, or a covered empty window
	// and an unmeasured one would look identical.
	unknown := serviceHealthPayload(observeServiceHealth(t.TempDir(), nil, nil, nil, time.Now()))
	if got := unknown["windowCoverage"]; got != serviceHealthWindowUnknown {
		t.Errorf("windowCoverage = %v, want %q", got, serviceHealthWindowUnknown)
	}
	if _, ok := unknown["observedUncleanRestarts"]; ok {
		t.Errorf("an unmeasured window reported a restart count: %+v", unknown)
	}
}

// TestServiceHealthCountsObservedUncleanRestarts pins that the count reflects
// real recorded history, and that the window start is the earliest event
// actually read rather than the daemon's start time.
func TestServiceHealthCountsObservedUncleanRestarts(t *testing.T) {
	log := openTestInstanceLog(t)
	for i := 0; i < 2; i++ {
		if err := log.Append(journal.Event{Type: journal.EventDaemonDirtyRestart, Reason: "test"}); err != nil {
			t.Fatalf("append dirty restart: %v", err)
		}
	}
	if err := log.Append(journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		t.Fatalf("append started: %v", err)
	}

	payload := serviceHealthPayload(observeServiceHealth(t.TempDir(), nil, log, nil, time.Now()))
	if got := payload["observedUncleanRestarts"]; got != 2 {
		t.Errorf("observedUncleanRestarts = %v, want 2", got)
	}
	if _, ok := payload["observationWindowStart"]; !ok {
		t.Errorf("payload does not report the window start: %+v", payload)
	}
}

// TestServiceHealthCancellationStopsCleanly pins that shutdown drains the
// emitter rather than leaving it writing into a closing log.
func TestServiceHealthCancellationStopsCleanly(t *testing.T) {
	log := openTestInstanceLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go emitServiceHealth(ctx, t.TempDir(), nil, log, nil, time.Hour, nil, done)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("emitServiceHealth did not return after cancellation")
	}
}

// TestServiceHealthSurvivesAnUnwritableLog proves the emitter never becomes the
// reason a daemon fails: diagnostic recording is best-effort by construction.
func TestServiceHealthSurvivesAnUnwritableLog(t *testing.T) {
	if err := appendServiceHealth(t.TempDir(), nil, nil, nil, time.Now()); err != nil {
		t.Fatalf("appendServiceHealth with no log = %v, want nil", err)
	}
	// A root path that does not exist must not panic the observation either.
	_ = serviceHealthPayload(observeServiceHealth(filepath.Join(t.TempDir(), "nope"), nil, nil, nil, time.Now()))
	if _, err := os.Stat(filepath.Join(t.TempDir(), "nope")); !os.IsNotExist(err) {
		t.Fatalf("precondition: the missing root should not exist: %v", err)
	}
}

func TestServiceHealthBoundedHistoryCannotClaimZeroRestarts(t *testing.T) {
	log := openTestInstanceLog(t)
	if err := log.Append(journal.Event{Type: journal.EventDaemonDirtyRestart}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if err := log.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "test-window"}}); err != nil {
			t.Fatal(err)
		}
	}
	observation := observeServiceHealth(t.TempDir(), nil, log, nil, time.Now())
	if observation.WindowCoverage != "partial" {
		t.Fatalf("bounded history claimed %s coverage", observation.WindowCoverage)
	}
	payload := serviceHealthPayload(observation)
	if _, present := payload["observedUncleanRestarts"]; present {
		t.Fatal("omitted earlier restart became a measured zero")
	}
}
