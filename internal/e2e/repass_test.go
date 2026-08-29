package e2e

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestGateVerdictPointerName(t *testing.T) {
	if got := GateVerdictPointerName("reviewer"); got != "reviewer.verdict" {
		t.Fatalf("GateVerdictPointerName(reviewer) = %q, want %q", got, "reviewer.verdict")
	}
}

func TestAssertRepassFreshNodeWithVerdictPass(t *testing.T) {
	original := placedAttempt(1, "initial", "pod-original", "node-a", "linux")
	repass := placedAttempt(2, string(journal.AttemptPolicy), "pod-repass", "node-b", "linux")
	pointers := []apiv1.ContextPointer{{Name: "reviewer.verdict"}}

	got := AssertRepassFreshNodeWithVerdict("reviewer", original, repass, pointers)
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

func TestAssertRepassFreshNodeWithVerdictFailsWrongClass(t *testing.T) {
	original := placedAttempt(1, "initial", "pod-original", "node-a", "linux")
	repass := placedAttempt(2, "initial", "pod-repass", "node-b", "linux") // wrong: should be "policy"
	pointers := []apiv1.ContextPointer{{Name: "reviewer.verdict"}}

	got := AssertRepassFreshNodeWithVerdict("reviewer", original, repass, pointers)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (repass is work, not weather)", got.Verdict)
	}
}

func TestAssertRepassFreshNodeWithVerdictFailsSameNode(t *testing.T) {
	original := placedAttempt(1, "initial", "pod-original", "node-a", "linux")
	repass := placedAttempt(2, string(journal.AttemptPolicy), "pod-repass", "node-a", "linux")
	pointers := []apiv1.ContextPointer{{Name: "reviewer.verdict"}}

	got := AssertRepassFreshNodeWithVerdict("reviewer", original, repass, pointers)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (same node)", got.Verdict)
	}
}

func TestAssertRepassFreshNodeWithVerdictFailsMissingPointer(t *testing.T) {
	original := placedAttempt(1, "initial", "pod-original", "node-a", "linux")
	repass := placedAttempt(2, string(journal.AttemptPolicy), "pod-repass", "node-b", "linux")

	got := AssertRepassFreshNodeWithVerdict("reviewer", original, repass, nil)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (repass 'worked' by losing the gate's context)", got.Verdict)
	}
}

func TestAssertRepassFreshNodeWithVerdictInvalidWithoutPlacement(t *testing.T) {
	original := readservice.StageAttempt{Number: 1, Class: "initial"}
	repass := readservice.StageAttempt{Number: 2, Class: string(journal.AttemptPolicy)}
	got := AssertRepassFreshNodeWithVerdict("reviewer", original, repass, nil)
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (no placement recorded)", got.Verdict)
	}
}
