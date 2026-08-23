package goobernetes

import (
	"testing"

	"github.com/goobers/goobers/internal/readservice"
)

func ledgerOnly(stages ...string) LedgerTouching {
	set := make(map[string]bool, len(stages))
	for _, s := range stages {
		set[s] = true
	}
	return func(stage string) bool { return set[stage] }
}

func TestAssertNoLedgerTouchingOnWindowsPass(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "claim-backlog-item", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-1", "node-a", "linux")}},
		{Stage: "windows-build", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-2", "node-b", "windows")}},
	}
	got := AssertNoLedgerTouchingOnWindows(stages, ledgerOnly("claim-backlog-item"))
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

func TestAssertNoLedgerTouchingOnWindowsCatchesViolation(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "claim-backlog-item", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-1", "node-a", "windows")}},
	}
	got := AssertNoLedgerTouchingOnWindows(stages, ledgerOnly("claim-backlog-item"))
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (ledger-touching stage placed on windows)", got.Verdict)
	}
}

func TestAssertNoLedgerTouchingOnWindowsInvalidWhenNoLedgerStageExercised(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "windows-build", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-1", "node-a", "windows")}},
	}
	got := AssertNoLedgerTouchingOnWindows(stages, ledgerOnly("claim-backlog-item"))
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (no ledger-touching stage was exercised)", got.Verdict)
	}
}

func TestAssertNoLedgerTouchingOnWindowsInvalidWithoutPredicate(t *testing.T) {
	stages := []readservice.AttemptList{{Stage: "x", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "p", "n", "linux")}}}
	got := AssertNoLedgerTouchingOnWindows(stages, nil)
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (no predicate)", got.Verdict)
	}
}
