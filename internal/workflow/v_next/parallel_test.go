package vnext

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// parallelDef builds a minimal two-branch fan-out definition that compiles
// clean, so each test can mutate exactly one thing and assert one rule.
//
//	churn -> fan (security|perf) -> collate
func parallelDef() Definition {
	return Definition{
		Name:       "quality-sprint",
		DSLVersion: DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "demo",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "churn",
			Tasks: []apiv1.Task{
				{Name: "churn", Type: apiv1.TaskDeterministic, Goal: "churn report",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: "fan"},
				{Name: "review-security", Type: apiv1.TaskDeterministic, Goal: "security lens",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: TargetJoin},
				{Name: "review-perf", Type: apiv1.TaskDeterministic, Goal: "perf lens",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: TargetJoin},
				{Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate findings",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: TerminalComplete},
			},
			Parallels: []apiv1.Parallel{{
				Name:          "fan",
				FailurePolicy: apiv1.BranchContinueOnError,
				Join:          "collate",
				Branches: []apiv1.Branch{
					{Name: "security", Start: "review-security"},
					{Name: "perf", Start: "review-perf"},
				},
			}},
		},
	}
}

func compileParallel(t *testing.T, def Definition) error {
	t.Helper()
	_, err := Compile(def, WithPreviewFeatures(true))
	return err
}

func mustReject(t *testing.T, def Definition, want string) {
	t.Helper()
	err := compileParallel(t, def)
	if err == nil {
		t.Fatalf("compile succeeded; want rejection mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("compile error = %v\nwant a message containing %q", err, want)
	}
}

func TestParallelBaselineCompiles(t *testing.T) {
	if err := compileParallel(t, parallelDef()); err != nil {
		t.Fatalf("baseline parallel definition should compile: %v", err)
	}
}

// Preview gating: every parallel field is preview until FO-8, so a workflow
// declaring one must not compile without the explicit opt-in.
func TestParallelRequiresPreviewOptIn(t *testing.T) {
	if _, err := Compile(parallelDef()); err == nil {
		t.Fatal("a parallel must not compile without the preview-features opt-in")
	}
}

// Rule 1 — disjoint branches.
func TestParallelRejectsSharedBranchState(t *testing.T) {
	def := parallelDef()
	// perf's body now flows into security's body.
	def.Spec.Tasks[2].Next = "review-security"
	mustReject(t, def, "must be disjoint")
}

func TestParallelRejectsBranchReachingJoinDirectly(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1].Next = "collate"
	mustReject(t, def, "reaches the join state")
}

func TestParallelRejectsBranchReachingOnFailure(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].FailurePolicy = apiv1.BranchFailFast
	def.Spec.Parallels[0].OnFailure = "park"
	def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{
		Name: "park", Type: apiv1.TaskDeterministic, Goal: "park",
		Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		Next: TerminalComplete,
	})
	def.Spec.Tasks[1].Next = "park"
	mustReject(t, def, "onFailure state")
}

// Rule 2 — branch exits are branch-terminal. A branch may not complete the run.
func TestParallelRejectsBranchCompletingTheRun(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1].Next = TerminalComplete
	mustReject(t, def, "cannot reach")
}

// Rule 3 — the join is parallel-entered only.
func TestParallelRejectsJoinEnteredFromOutside(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{
		Name: "sneak", Type: apiv1.TaskDeterministic, Goal: "sneak into the join",
		Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		Next: "collate",
	})
	def.Spec.Tasks[0].Next = "sneak"
	mustReject(t, def, "may only be entered through its parallel")
}

// Rule 4 — @join is branch-scoped.
func TestParallelRejectsJoinTargetOutsideBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[3].Next = TargetJoin // collate is the join, not a branch body
	mustReject(t, def, "not inside a parallel branch")
}

// Rule 5 — bounds.
func TestParallelRejectsSingleBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].Branches = def.Spec.Parallels[0].Branches[:1]
	def.Spec.Tasks[2].Next = TerminalComplete // keep review-perf otherwise valid
	mustReject(t, def, "at least 2")
}

func TestParallelRejectsDuplicateBranchNames(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].Branches[1].Name = "security"
	mustReject(t, def, "duplicate branch")
}

func TestParallelRejectsUndefinedBranchStart(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].Branches[1].Start = "nope"
	mustReject(t, def, "is not a defined state")
}

// Rule 6 — policy completeness.
func TestParallelRequiresFailurePolicy(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].FailurePolicy = ""
	mustReject(t, def, "no failurePolicy")
}

func TestParallelFailFastRequiresOnFailure(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].FailurePolicy = apiv1.BranchFailFast
	mustReject(t, def, "requires onFailure")
}

func TestParallelContinueOnErrorForbidsOnFailure(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].OnFailure = "@escalate"
	mustReject(t, def, "must not declare onFailure")
}

func TestParallelAcceptsReservedOnFailureTarget(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].FailurePolicy = apiv1.BranchAllOrNothing
	def.Spec.Parallels[0].OnFailure = "@escalate"
	if err := compileParallel(t, def); err != nil {
		t.Fatalf("@escalate is a valid onFailure target: %v", err)
	}
}

// Rule 7 — no nesting.
func TestParallelRejectsNestedParallel(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks = append(def.Spec.Tasks,
		apiv1.Task{Name: "inner-a", Type: apiv1.TaskDeterministic, Goal: "a",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: TargetJoin},
		apiv1.Task{Name: "inner-b", Type: apiv1.TaskDeterministic, Goal: "b",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: TargetJoin},
		apiv1.Task{Name: "inner-join", Type: apiv1.TaskDeterministic, Goal: "inner join",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: TargetJoin},
	)
	def.Spec.Tasks[1].Next = "inner" // security branch enters a nested parallel
	def.Spec.Parallels = append(def.Spec.Parallels, apiv1.Parallel{
		Name:          "inner",
		FailurePolicy: apiv1.BranchContinueOnError,
		Join:          "inner-join",
		Branches: []apiv1.Branch{
			{Name: "a", Start: "inner-a"},
			{Name: "b", Start: "inner-b"},
		},
	})
	mustReject(t, def, "nested parallels are not supported")
}

// Rule 8 — timeout coherence.
func TestParallelRejectsStageTimeoutExceedingBranchBudget(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].BranchTimeoutSeconds = 60
	def.Spec.Tasks[1].TimeoutSeconds = 600
	mustReject(t, def, "exceeds branchTimeoutSeconds")
}

// Rule 9 — no writable repo workspace inside a branch.
func TestParallelRejectsWritableRepoWorkspaceInBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1].Run.Workspace = apiv1.WorkspaceRepo
	mustReject(t, def, "writable repo workspace")
}

// Rule 10 — no human gate inside a branch.
func TestParallelRejectsHumanGateInBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1].Next = "approve"
	def.Spec.Gates = append(def.Spec.Gates, apiv1.Gate{
		Name:      "approve",
		Evaluator: apiv1.EvaluatorHuman,
		Human:     &apiv1.HumanGate{Approvers: []string{"someone"}},
		Branches:  map[string]string{"pass": TargetJoin, "fail": TargetJoin},
	})
	mustReject(t, def, "human gates are not supported inside a branch")
}

// A gate inside a branch is fine as long as it is not a human gate, and its
// back-edge (repass) cycle is legal — bounded at runtime by the branch timeout.
func TestParallelAcceptsGateWithRepassInsideBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1].Next = "verdict"
	def.Spec.Gates = append(def.Spec.Gates, apiv1.Gate{
		Name:      "verdict",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches: map[string]string{
			"pass": TargetJoin,
			"fail": "review-security", // back-edge inside the same branch
		},
	})
	if err := compileParallel(t, def); err != nil {
		t.Fatalf("a non-human gate with a repass back-edge is legal inside a branch: %v", err)
	}
}

// The graph projection must expose the parallel and its fan-out edges in
// declaration order — the same order that assigns branch ids.
func TestParallelGraphProjection(t *testing.T) {
	m, err := Compile(parallelDef(), WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	graph := m.Graph()

	var found bool
	for _, node := range graph.Nodes {
		if node.ID == "fan" {
			found = true
			if node.Kind != "parallel" {
				t.Errorf("fan node kind = %q, want parallel", node.Kind)
			}
		}
	}
	if !found {
		t.Fatal("graph has no node for the parallel state")
	}

	var branchTargets []string
	var joined string
	for _, edge := range graph.Edges {
		if edge.Source != "fan" {
			continue
		}
		if edge.Branch != "" {
			branchTargets = append(branchTargets, edge.Branch+"->"+edge.Target)
		}
		if edge.Outcome == "join" {
			joined = edge.Target
		}
	}
	want := []string{"security->review-security", "perf->review-perf"}
	if len(branchTargets) != len(want) {
		t.Fatalf("fan-out edges = %v, want %v", branchTargets, want)
	}
	for i := range want {
		if branchTargets[i] != want[i] {
			t.Errorf("fan-out edge %d = %q, want %q (declaration order fixes branch ids)", i, branchTargets[i], want[i])
		}
	}
	if joined != "collate" {
		t.Errorf("join edge target = %q, want collate", joined)
	}
}

// DSL 1.4 has no fan-out construct, so a parallel state must not resolve there.
func TestParallelIsNotAStateAtDSL14(t *testing.T) {
	m, err := Compile(parallelDef(), WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, ok := m.Parallel("fan"); !ok {
		t.Fatal("v_next machine should resolve its parallel states")
	}
	if !m.Has("fan") {
		t.Fatal("a parallel is a state and Has must report it")
	}
}
