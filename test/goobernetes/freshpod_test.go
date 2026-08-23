package goobernetes

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func placedAttempt(number int, class string, pod, node, os string) readservice.StageAttempt {
	return readservice.StageAttempt{
		Number: number,
		Class:  class,
		Placement: &journal.Placement{
			Runner: "runner-a", Pod: pod, Node: node, OS: os,
		},
	}
}

func TestAssertFreshPodPerAttemptPass(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "implement", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-implement-1", "node-a", "linux")}},
		{Stage: "local-ci", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-localci-1", "node-b", "linux")}},
	}
	got := AssertFreshPodPerAttempt(stages)
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

func TestAssertFreshPodPerAttemptDetectsDuplicate(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "implement", Attempts: []readservice.StageAttempt{placedAttempt(1, "initial", "pod-shared", "node-a", "linux")}},
		{Stage: "repass-gate", Attempts: []readservice.StageAttempt{placedAttempt(1, "policy", "pod-shared", "node-a", "linux")}},
	}
	got := AssertFreshPodPerAttempt(stages)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (duplicate pod)", got.Verdict)
	}
}

func TestAssertFreshPodPerAttemptInvalidOnMissingPlacement(t *testing.T) {
	stages := []readservice.AttemptList{
		{Stage: "implement", Attempts: []readservice.StageAttempt{{Number: 1, Class: "initial"}}},
	}
	got := AssertFreshPodPerAttempt(stages)
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (no placement provenance recorded)", got.Verdict)
	}
}

func TestAssertFreshPodPerAttemptInvalidOnEmptyInput(t *testing.T) {
	if got := AssertFreshPodPerAttempt(nil); got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid on nil input", got.Verdict)
	}
	if got := AssertFreshPodPerAttempt([]readservice.AttemptList{{Stage: "x"}}); got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid on zero attempts", got.Verdict)
	}
}
