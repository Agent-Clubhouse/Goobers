package goobernetes

import (
	"testing"
	"time"
)

func TestAssertLiveVisibilityPass(t *testing.T) {
	start := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	terminal := start.Add(5 * time.Minute)
	observations := []StageTransitionObservation{
		{Source: "sse", Stage: "implement", ObservedAt: start.Add(1 * time.Minute)},
	}
	got := AssertLiveVisibility(observations, terminal)
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

// TestAssertLiveVisibilityFailsTerminalOnly is S8's explicit named fail:
// "Terminal-only visibility — today's closed-run projection shape — is an
// explicit fail."
func TestAssertLiveVisibilityFailsTerminalOnly(t *testing.T) {
	terminal := time.Date(2026, 8, 22, 10, 5, 0, 0, time.UTC)
	observations := []StageTransitionObservation{
		{Source: "portal-screenshot", Stage: "implement", ObservedAt: terminal.Add(1 * time.Second)},
	}
	got := AssertLiveVisibility(observations, terminal)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (observation arrived after terminal)", got.Verdict)
	}
}

func TestAssertLiveVisibilityFailsWithNoObservations(t *testing.T) {
	got := AssertLiveVisibility(nil, time.Now())
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (no observation captured at all)", got.Verdict)
	}
}

func TestAssertLiveVisibilityInvalidWithoutTerminalTimestamp(t *testing.T) {
	observations := []StageTransitionObservation{{Source: "sse", Stage: "x", ObservedAt: time.Now()}}
	got := AssertLiveVisibility(observations, time.Time{})
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (no run terminal timestamp)", got.Verdict)
	}
}

func TestAssertLiveVisibilityInvalidWithUnstampedObservation(t *testing.T) {
	observations := []StageTransitionObservation{{Source: "sse", Stage: "x"}}
	got := AssertLiveVisibility(observations, time.Now())
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (observation has no timestamp)", got.Verdict)
	}
}
