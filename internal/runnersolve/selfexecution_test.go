package runnersolve

import "testing"

func TestSelfExecutionDeniedAllSolverPaths(t *testing.T) {
	inv := Inventory{SelfExecutionDenied: true, Runners: []Runner{{Name: "daemon-alias", Self: true}, {Name: "pod", Shell: true, Harnesses: []string{"copilot"}}}}
	stages := []StageRequirement{{Stage: "shell", Capabilities: []string{"run:shell"}}, {Stage: "review", StageClass: "agentic", ControlPlane: true}, {Stage: "check", StageClass: "deterministic", ControlPlane: true}}
	got := Solve(inv, stages)
	if len(got.Stages[0].Eligible) != 1 || got.Stages[0].Eligible[0] != "pod" || got.Stages[1].Unsat == nil || got.Stages[2].Unsat != nil {
		t.Fatalf("full inventory: %+v", got)
	}
	local := SolveExecutable(inv, stages)
	if local.Stages[0].Unsat == nil || local.Stages[1].Unsat == nil || local.Stages[2].Unsat != nil {
		t.Fatalf("local substrate bypass: %+v", local)
	}
}
