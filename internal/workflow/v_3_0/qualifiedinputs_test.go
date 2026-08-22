package v30

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// A linear pipeline: select -> gather -> apply. `apply` is two hops from
// `select`, which is precisely the shape that used to force an echo hop.
func qualifiedRefDef() Definition {
	return Definition{
		Name:       "pipeline",
		DSLVersion: DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "demo",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "select",
			Tasks: []apiv1.Task{
				{
					Name: "select", Type: apiv1.TaskDeterministic, Goal: "select one PR",
					Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Inputs:          map[string]string{"resultFile": "result.json"},
					ExpectedOutputs: []string{"selectedNumber"},
					Next:            "gather",
				},
				{
					Name: "gather", Type: apiv1.TaskDeterministic, Goal: "gather context",
					Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Inputs:          map[string]string{"resultFile": "result.json"},
					ExpectedOutputs: []string{"files"},
					Next:            "apply",
				},
				{
					Name: "apply", Type: apiv1.TaskDeterministic, Goal: "apply the verdict",
					Run:        &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					InputsFrom: map[string]string{"pullNumber": "select.selectedNumber"},
					Next:       TerminalComplete,
				},
			},
		},
	}
}

func TestQualifiedInputsFromCompilesAcrossTwoHops(t *testing.T) {
	def := qualifiedRefDef()
	if _, err := Compile(def, WithPreviewFeatures(true)); err != nil {
		t.Fatalf("a qualified reference two hops upstream should compile: %v", err)
	}
	if problems := CheckStageContracts(def); len(problems) > 0 {
		t.Fatalf("a valid qualified reference should raise no stage-contract problems, got %v", problems)
	}
}

// assertContractProblem asserts `goobers validate` surfaces a stage-contract
// problem containing want. These are validate-time diagnostics rather than
// Compile errors: Compile stays permissive so an already-shipped workflow can
// never stop loading because a new check was added.
func assertContractProblem(t *testing.T, def Definition, want string) {
	t.Helper()
	problems := CheckStageContracts(def)
	for _, p := range problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Fatalf("stage-contract problems = %v, want one containing %q", problems, want)
}

func TestQualifiedInputsFromRejectsUndeclaredOutput(t *testing.T) {
	def := qualifiedRefDef()
	def.Spec.Tasks[2].InputsFrom = map[string]string{"pullNumber": "select.nope"}
	assertContractProblem(t, def, "declares outputs")
}

// The load-bearing rule: a stage that runs on only SOME paths to the consumer
// would resolve in testing and fail in production on the other path.
func TestQualifiedInputsFromRejectsStageNotOnEveryPath(t *testing.T) {
	def := qualifiedRefDef()
	// Add a gate before `apply` whose fail branch skips `gather` entirely and
	// enters `apply` directly from a stage that never runs `select`.
	def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{
		Name: "sidecar", Type: apiv1.TaskDeterministic, Goal: "alternate entry",
		Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		Next: "apply",
	})
	def.Spec.Start = "entry"
	def.Spec.Gates = append(def.Spec.Gates, apiv1.Gate{
		Name:      "entry",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{"pass": "select", "fail": "sidecar"},
	})
	assertContractProblem(t, def, "does not run on every path")
}

func TestQualifiedInputsFromRejectsSelfReference(t *testing.T) {
	def := qualifiedRefDef()
	def.Spec.Tasks[2].InputsFrom = map[string]string{"x": "apply.something"}
	assertContractProblem(t, def, "from itself")
}

// A dotted value whose prefix is NOT a declared task stays a bare key, so an
// output literally named "a.b" keeps working. This is what makes the change
// backward-compatible.
func TestDottedValueWithUnknownPrefixStaysABareKey(t *testing.T) {
	def := qualifiedRefDef()
	def.Spec.Tasks[1].ExpectedOutputs = []string{"legacy.dotted"}
	def.Spec.Tasks[2].InputsFrom = map[string]string{"x": "legacy.dotted"}
	if _, err := Compile(def); err != nil {
		t.Fatalf("a dotted key whose prefix is not a stage must stay a bare key: %v", err)
	}
	if problems := CheckStageContracts(def); len(problems) > 0 {
		t.Fatalf("a legacy dotted key should raise no stage-contract problems, got %v", problems)
	}
}

// Banning dots in state names is what removes the need for escaping syntax.
func TestStateNamesMayNotContainDots(t *testing.T) {
	def := qualifiedRefDef()
	def.Spec.Tasks[1].Name = "gather.context"
	def.Spec.Tasks[0].Next = "gather.context"
	_, err := Compile(def, WithPreviewFeatures(true))
	if err == nil || !strings.Contains(err.Error(), "contains a dot") {
		t.Fatalf("Compile error = %v, want a dotted-state-name rejection", err)
	}
}

func TestGateNamesMayNotContainDots(t *testing.T) {
	def := qualifiedRefDef()
	def.Spec.Gates = append(def.Spec.Gates, apiv1.Gate{
		Name:      "check.thing",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
	})
	_, err := Compile(def, WithPreviewFeatures(true))
	if err == nil || !strings.Contains(err.Error(), "contains a dot") {
		t.Fatalf("Compile error = %v, want a dotted-gate-name rejection", err)
	}
}
