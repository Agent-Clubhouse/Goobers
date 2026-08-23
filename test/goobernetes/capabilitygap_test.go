package goobernetes

import (
	"testing"

	"github.com/goobers/goobers/internal/runnersolve"
)

// TestAssertCapabilityGapEnforcedAgainstRealSolve drives the ACTUAL
// runnersolve.Solve (internal/runnersolve/runnersolve.go), not a
// hand-built Result, so this proves the assertion helper against the real
// solver contract: a stage requiring an OS no declared runner claims must
// come back unsatisfiable.
func TestAssertCapabilityGapEnforcedAgainstRealSolve(t *testing.T) {
	inventory := runnersolve.Inventory{
		Runners: []runnersolve.Runner{
			{Name: "linux-runner", OS: runnersolve.OSLinux},
		},
	}
	stages := []runnersolve.StageRequirement{
		{Stage: "macos-only-stage", OS: runnersolve.OSMacOS},
	}
	result := runnersolve.Solve(inventory, stages)

	got := AssertCapabilityGapEnforced(result, "macos-only-stage")
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

func TestAssertCapabilityGapEnforcedFailsWhenSatisfiable(t *testing.T) {
	inventory := runnersolve.Inventory{
		Runners: []runnersolve.Runner{{Name: "linux-runner", OS: runnersolve.OSLinux}},
	}
	stages := []runnersolve.StageRequirement{{Stage: "linux-stage", OS: runnersolve.OSLinux}}
	result := runnersolve.Solve(inventory, stages)

	got := AssertCapabilityGapEnforced(result, "linux-stage")
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (the stage IS satisfiable, so the gap was never caught)", got.Verdict)
	}
}

func TestAssertCapabilityGapEnforcedInvalidOnEmptyResult(t *testing.T) {
	got := AssertCapabilityGapEnforced(runnersolve.Result{}, "some-stage")
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid", got.Verdict)
	}
}

func TestAssertCapabilityGapEnforcedInvalidWithoutTarget(t *testing.T) {
	inventory := runnersolve.Inventory{Runners: []runnersolve.Runner{{Name: "r", OS: runnersolve.OSLinux}}}
	result := runnersolve.Solve(inventory, []runnersolve.StageRequirement{{Stage: "s", OS: runnersolve.OSMacOS}})
	got := AssertCapabilityGapEnforced(result, "")
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (no target stage named)", got.Verdict)
	}
}
