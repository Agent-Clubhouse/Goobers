package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestDaemonProbeStateLivenessGraceBeforeFirstTick locks the pre-first-tick
// grace window: a long legitimate crash-resume must not read as a wedged
// main loop, regardless of how stale (or absent) the heartbeat is.
func TestDaemonProbeStateLivenessGraceBeforeFirstTick(t *testing.T) {
	var schedulerTicked atomic.Bool
	var lastTickAtNanos atomic.Int64
	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	state := &daemonProbeState{
		schedulerTicked: &schedulerTicked,
		lastTickAtNanos: &lastTickAtNanos,
		livenessTimeout: time.Minute,
		now:             func() time.Time { return now },
	}
	if !state.liveness() {
		t.Fatal("liveness() before the scheduler's first tick must default healthy (startup grace)")
	}
}

// TestDaemonProbeStateLivenessReflectsHeartbeatStaleness is the direct,
// no-daemon-required test of #3806's liveness closure the reviewers asked
// for: it must go unhealthy once the heartbeat exceeds livenessTimeout, and
// this is exactly the behavior a hardcoded `return true` (the mutation
// the PR's own ablation evidence used) would silently drop.
func TestDaemonProbeStateLivenessReflectsHeartbeatStaleness(t *testing.T) {
	var schedulerTicked atomic.Bool
	var lastTickAtNanos atomic.Int64
	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	state := &daemonProbeState{
		schedulerTicked: &schedulerTicked,
		lastTickAtNanos: &lastTickAtNanos,
		livenessTimeout: time.Minute,
		now:             func() time.Time { return now },
	}

	schedulerTicked.Store(true)
	lastTickAtNanos.Store(now.UnixNano())
	if !state.liveness() {
		t.Fatal("liveness() with a fresh heartbeat must be healthy")
	}

	now = now.Add(2 * time.Minute) // exceeds the 1-minute livenessTimeout
	if state.liveness() {
		t.Fatal("liveness() with a heartbeat stale beyond livenessTimeout must be unhealthy — a wedged main loop must be observable")
	}

	now = now.Add(-90 * time.Second) // back within budget (30s stale)
	if !state.liveness() {
		t.Fatal("liveness() must recover once a fresh-enough heartbeat is observed again")
	}
}

// TestDaemonProbeStateReadinessReflectsReadyGate is the direct test of
// #3806's readiness closure: Ready must reflect the `ready` gate, not
// default true. A hardcoded `Ready: true` (this PR's own literal
// regression, per issue #3806) would pass every existing test that only
// exercises the post-startup happy path; this one specifically starts from
// ready=false.
func TestDaemonProbeStateReadinessReflectsReadyGate(t *testing.T) {
	var ready, configLoaded, stateOpen, resumeComplete, sweepsStarted atomic.Bool
	state := &daemonProbeState{
		ready:          &ready,
		configLoaded:   &configLoaded,
		stateOpen:      &stateOpen,
		resumeComplete: &resumeComplete,
		sweepsStarted:  &sweepsStarted,
	}

	got := state.readiness()
	if got.Ready {
		t.Fatal("readiness() must be false before the ready gate flips, not default true")
	}
	for name, value := range got.Checks {
		if value {
			t.Fatalf("check %q = true before anything ran, want false", name)
		}
	}

	ready.Store(true)
	configLoaded.Store(true)
	stateOpen.Store(true)
	resumeComplete.Store(true)
	sweepsStarted.Store(true)

	got = state.readiness()
	if !got.Ready {
		t.Fatal("readiness() must flip true once the ready gate is set")
	}
	for _, name := range []string{"configLoaded", "stateOpen", "resumeComplete", "sweepsStarted"} {
		if !got.Checks[name] {
			t.Fatalf("check %q = false once every subsystem flipped true, checks = %+v", name, got.Checks)
		}
	}
}
