package shutdown

import (
	"testing"
	"time"
)

// TestDrainWithEscalation_EscalatesWhenDrainOverruns is the AC#4 shutdown-
// sequence test driven by a fake trigger: the drain deliberately outlasts the
// grace budget and only returns once escalation (the KillTree stand-in) fires,
// proving trigger → drain → escalate-on-timeout.
func TestDrainWithEscalation_EscalatesWhenDrainOverruns(t *testing.T) {
	killed := make(chan struct{})
	inTime := DrainWithEscalation(
		time.Millisecond,
		func() { <-killed },      // drain wedges until the hard-kill releases it
		func() { close(killed) }, // escalate: the proc.Tree.Kill stand-in
	)
	if inTime {
		t.Fatal("DrainWithEscalation reported drainedInTime, want false (drain overran grace)")
	}
	select {
	case <-killed:
	default:
		t.Fatal("escalate was not invoked after the drain overran grace")
	}
}

// TestDrainWithEscalation_NoEscalationWhenDrainFinishesInTime locks the graceful
// case: a drain that beats the grace budget must never escalate.
func TestDrainWithEscalation_NoEscalationWhenDrainFinishesInTime(t *testing.T) {
	escalated := make(chan struct{}, 1)
	inTime := DrainWithEscalation(
		time.Second,
		func() {}, // drains immediately
		func() { escalated <- struct{}{} },
	)
	if !inTime {
		t.Fatal("DrainWithEscalation reported not-in-time, want true (drain beat grace)")
	}
	select {
	case <-escalated:
		t.Fatal("escalate fired although the drain finished within grace")
	default:
	}
}

// TestDrainWithEscalation_NilEscalateWaitsOut ensures a nil escalate is tolerated
// (no panic) and DrainWithEscalation still returns once the drain completes.
func TestDrainWithEscalation_NilEscalateWaitsOut(t *testing.T) {
	release := make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		// Grace elapses long before release, so this exercises the
		// overrun-then-nil-escalate path without panicking.
		result <- DrainWithEscalation(time.Millisecond, func() { <-release }, nil)
	}()
	close(release)
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("DrainWithEscalation did not return after the drain completed")
	}
}
