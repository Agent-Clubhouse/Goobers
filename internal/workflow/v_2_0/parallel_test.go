package v20

import (
	"reflect"
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

func fanInDef() Definition {
	def := parallelDef()
	def.Spec.Tasks[1].ExpectedOutputs = []string{"findings"}
	def.Spec.Tasks[2].ExpectedOutputs = []string{"findings"}
	def.Spec.Tasks[3].InputsFrom = map[string]string{
		"security": "fan.security.review-security.findings",
		"perf":     "fan.perf.findings",
	}
	return def
}

func TestParallelBranchQualifiedInputsFromCompiles(t *testing.T) {
	if err := compileParallel(t, fanInDef()); err != nil {
		t.Fatalf("branch-qualified inputsFrom should compile: %v", err)
	}
}

func TestParallelJoinPreservesStageQualifiedDottedOutputKeys(t *testing.T) {
	def := fanInDef()
	def.Spec.Tasks[0].ExpectedOutputs = []string{"legacy.dotted"}
	def.Spec.Tasks[3].InputsFrom["legacy"] = "churn.legacy.dotted"
	if err := compileParallel(t, def); err != nil {
		t.Fatalf("stage-qualified dotted output at a join should compile: %v", err)
	}
}

func TestParallelBranchQualifiedInputsFromRejectsUnknownReferences(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "parallel", value: "missing.security.review-security.findings", want: "unknown parallel"},
		{name: "branch", value: "fan.missing.review-security.findings", want: "unknown branch"},
		{name: "stage", value: "fan.security.missing.findings", want: "unknown stage"},
		{name: "output", value: "fan.security.review-security.missing", want: "declares outputs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := fanInDef()
			def.Spec.Tasks[3].InputsFrom = map[string]string{"findings": tc.value}
			mustReject(t, def, tc.want)
		})
	}
}

func TestParallelBranchQualifiedInputsFromRejectsAmbiguousShorthand(t *testing.T) {
	def := fanInDef()
	def.Spec.Tasks[1].Next = "choose-security"
	def.Spec.Tasks = append(def.Spec.Tasks,
		apiv1.Task{
			Name: "security-a", Type: apiv1.TaskDeterministic, Goal: "security a",
			Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			ExpectedOutputs: []string{"findings"},
			Next:            TargetJoin,
		},
		apiv1.Task{
			Name: "security-b", Type: apiv1.TaskDeterministic, Goal: "security b",
			Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			ExpectedOutputs: []string{"findings"},
			Next:            TargetJoin,
		},
	)
	def.Spec.Gates = append(def.Spec.Gates, apiv1.Gate{
		Name:      "choose-security",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{"pass": "security-a", "fail": "security-b"},
	})
	def.Spec.Tasks[3].InputsFrom = map[string]string{"security": "fan.security.findings"}
	mustReject(t, def, "branch has 2 join-terminal stages, qualify the stage")
}

func TestParallelBranchQualifiedInputsFromRequiresProducerOnEveryJoinPath(t *testing.T) {
	def := fanInDef()
	def.Spec.Tasks[1].Next = "choose-security"
	def.Spec.Tasks = append(def.Spec.Tasks,
		apiv1.Task{
			Name: "security-a", Type: apiv1.TaskDeterministic, Goal: "security a",
			Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			ExpectedOutputs: []string{"findings"},
			Next:            TargetJoin,
		},
		apiv1.Task{
			Name: "security-b", Type: apiv1.TaskDeterministic, Goal: "security b",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: TargetJoin,
		},
	)
	def.Spec.Gates = append(def.Spec.Gates, apiv1.Gate{
		Name:      "choose-security",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{"pass": "security-a", "fail": "security-b"},
	})
	def.Spec.Tasks[3].InputsFrom = map[string]string{
		"security": "fan.security.security-a.findings",
	}
	mustReject(t, def, "does not run on every successful path to @join")
}

func TestParallelAndBranchNamesMayNotContainDots(t *testing.T) {
	def := parallelDef()
	def.Spec.Parallels[0].Name = "fan.out"
	def.Spec.Tasks[0].Next = "fan.out"
	mustReject(t, def, "parallel name")

	def = parallelDef()
	def.Spec.Parallels[0].Branches[0].Name = "security.deep"
	mustReject(t, def, "branch name")
}

// Parallel fields were preview-gated until static fan-out/fan-in graduated to
// GA in #1939. A parallel now compiles without the preview-features opt-in.
func TestParallelCompilesWithoutPreviewOptIn(t *testing.T) {
	if _, err := Compile(parallelDef()); err != nil {
		t.Fatalf("a GA parallel should compile without the preview-features opt-in: %v", err)
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

// Rule 9 must catch the unset default too, not just an explicit
// `workspace: repo` — Run.Workspace == "" resolves to the writable repo
// worktree the same as an explicit "repo" (internal/runner.taskWorkspaceMode).
func TestParallelRejectsDefaultWorkspaceInBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1].Run.Workspace = ""
	mustReject(t, def, "writable repo workspace")
}

// Rule 9 must also validate an AGENTIC task's Workspace field (Run is nil for
// an agentic task, so it has no Run.Workspace to check at all) — both an
// explicit "repo" and the unset default.
func TestParallelRejectsWritableWorkspaceOnAgenticTaskInBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1] = apiv1.Task{
		Name: "review-security", Type: apiv1.TaskAgentic, Goal: "security lens",
		Goober: "reviewer", Next: TargetJoin,
	}
	mustReject(t, def, "writable repo workspace")
}

// Rule 9 must also validate an agentic GATE's Agentic.Workspace field inside a
// branch — unset resolves to the writable repo worktree just like an agentic
// task's unset Workspace.
func TestParallelRejectsWritableWorkspaceOnAgenticGateInBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1].Next = "verdict"
	def.Spec.Gates = append(def.Spec.Gates, apiv1.Gate{
		Name:      "verdict",
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
		Branches:  map[string]string{"pass": TargetJoin, "fail": TargetJoin},
	})
	mustReject(t, def, "writable repo workspace")
}

// An agentic task/gate that explicitly opts into repo-readonly stays legal —
// rule 9 must not overreach into rejecting the workspace FO-4 added for this.
func TestParallelAcceptsRepoReadonlyAgenticTaskInBranch(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[1] = apiv1.Task{
		Name: "review-security", Type: apiv1.TaskAgentic, Goal: "security lens",
		Goober: "reviewer", Workspace: apiv1.WorkspaceRepoReadOnly, Next: TargetJoin,
	}
	if err := compileParallel(t, def); err != nil {
		t.Fatalf("repo-readonly agentic task should compile: %v", err)
	}
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
	var joinSources []string
	for _, edge := range graph.Edges {
		if edge.Target == "collate" {
			joinSources = append(joinSources, edge.Source)
		}
		if edge.Source == "fan" && edge.Branch != "" {
			branchTargets = append(branchTargets, edge.Branch+"->"+edge.Target)
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
	wantJoinSources := []string{"review-security", "review-perf"}
	if !reflect.DeepEqual(joinSources, wantJoinSources) {
		t.Errorf("join edge sources = %v, want %v", joinSources, wantJoinSources)
	}
}

// DSL 1.4 has no fan-out construct, so a parallel state must not resolve there.
func TestParallelIsNotAStateAtDSL14(t *testing.T) {
	m, err := Compile(parallelDef(), WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, ok := m.Parallel("fan"); !ok {
		t.Fatal("v_2_0 machine should resolve its parallel states")
	}
	if !m.Has("fan") {
		t.Fatal("a parallel is a state and Has must report it")
	}
}
