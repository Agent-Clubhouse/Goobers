package goobernetes

import (
	"testing"

	"github.com/goobers/goobers/internal/readservice"
)

func TestAssertOSHopPass(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "implement", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-1", "node-a", "linux")}},
		{Stage: "windows-stage", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-2", "node-b", "windows")}},
	}
	got := AssertOSHop(stages, true)
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

func TestAssertOSHopFailsWhenNoWindows(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "implement", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-1", "node-a", "linux")}},
	}
	got := AssertOSHop(stages, true)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (no windows hop)", got.Verdict)
	}
}

func TestAssertOSHopFailsWhenRunNeverCompleted(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "implement", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-1", "node-a", "linux")}},
		{Stage: "windows-stage", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-2", "node-b", "windows")}},
	}
	got := AssertOSHop(stages, false)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (run never completed)", got.Verdict)
	}
}

func TestAssertOSHopInvalidOnNoPlacementAtAll(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "implement", Attempts: []readservice.StageAttempt{{Number: 1, Class: "initial"}}},
	}
	got := AssertOSHop(stages, true)
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid", got.Verdict)
	}
}
