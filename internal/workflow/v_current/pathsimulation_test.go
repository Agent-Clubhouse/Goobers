package vcurrent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// electLanderIncidentFixture reproduces #900's exact shape: elect-lander
// declares expectedOutputs including reviewDigest, elect-gate routes its
// failing outcome straight to apply-verdict, and apply-verdict reads
// reviewDigest through inputsFrom. With resultFileOnLander false, elect-lander
// is a shell stage with no channel to emit anything — the live bug.
func electLanderIncidentFixture(resultFileOnLander bool) Definition {
	lander := apiv1.Task{
		Name: "elect-lander", Type: apiv1.TaskDeterministic,
		Run:             &apiv1.DeterministicRun{Command: []string{"goobers", "elect-lander"}},
		ExpectedOutputs: []string{"elected", "selectedNumber", "reviewDigest"},
		Next:            "elect-gate",
	}
	if resultFileOnLander {
		lander.Inputs = map[string]string{"resultFile": "election.json"}
	}
	return Definition{Name: "merge-review", Spec: apiv1.WorkflowSpec{
		Start: "elect-lander",
		Tasks: []apiv1.Task{
			lander,
			{Name: "merge-pr", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"goobers", "merge-pr"}}},
			{
				Name: "apply-verdict", Type: apiv1.TaskDeterministic,
				Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "apply-verdict"}},
				InputsFrom: map[string]string{"reviewDigest": "reviewDigest"},
			},
		},
		Gates: []apiv1.Gate{{
			Name: "elect-gate", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "output-equals", Params: map[string]string{"key": "elected", "equals": "true"}},
			Branches:  map[string]string{"pass": "merge-pr", "fail": "apply-verdict"},
		}},
	}}
}

// TestPathSimulationReproducesElectLanderIncident pins #900's incident as a
// regression: on the elect-gate:fail path, apply-verdict cannot resolve
// reviewDigest because elect-lander (a shell stage with no resultFile) emits
// nothing, even though it DECLARES reviewDigest — the case
// CheckStageContracts' own unsatisfiableInputsFromProblems trusts the
// declared expectedOutputs and would miss on this axis alone (it is only
// caught there via the separate undeclaredResultFileProblems check). Path
// simulation catches it directly, by tracking what elect-lander actually
// emits on this concrete path.
func TestPathSimulationReproducesElectLanderIncident(t *testing.T) {
	problems := CheckPathSimulation(electLanderIncidentFixture(false))
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", problems)
	}
	for _, want := range []string{"apply-verdict", "reviewDigest", "elect-lander", "elect-gate"} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("problem = %q, want it to mention %q", problems[0], want)
		}
	}
}

// TestPathSimulationClearsOnceResultFileDeclared is the actual #900 fix:
// declaring elect-lander's resultFile makes its expectedOutputs real, and the
// same path resolves.
func TestPathSimulationClearsOnceResultFileDeclared(t *testing.T) {
	if problems := CheckPathSimulation(electLanderIncidentFixture(true)); len(problems) != 0 {
		t.Fatalf("problems = %v, want none once resultFile is declared", problems)
	}
}

// TestPathSimulationReportsExactPath is the #562-shaped case: a stage
// reachable by two branches, where only one branch's predecessor emits the
// key it reads. Unlike the per-edge check (which names the predecessor task),
// path simulation must report the exact sequence of hops that produces the
// failure.
func TestPathSimulationReportsExactPath(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "produce",
		Tasks: []apiv1.Task{
			{
				Name: "produce", Type: apiv1.TaskDeterministic,
				Run:             &apiv1.DeterministicRun{Command: []string{"goobers", "produce"}},
				Inputs:          map[string]string{"resultFile": "r.json"},
				ExpectedOutputs: []string{"digest"},
				Next:            "branch",
			},
			{
				Name: "detour", Type: apiv1.TaskDeterministic,
				Run:             &apiv1.DeterministicRun{Command: []string{"goobers", "detour"}},
				Inputs:          map[string]string{"resultFile": "d.json"},
				ExpectedOutputs: []string{"somethingElse"},
				Next:            "consume",
			},
			{
				Name: "consume", Type: apiv1.TaskDeterministic,
				Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "consume"}},
				InputsFrom: map[string]string{"digest": "digest"},
			},
		},
		Gates: []apiv1.Gate{{
			Name: "branch", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "output-equals", Params: map[string]string{"key": "digest", "equals": "x"}},
			Branches:  map[string]string{"pass": "consume", "fail": "detour"},
		}},
	}}
	problems := CheckPathSimulation(def)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one (the detour branch only)", problems)
	}
	if !strings.Contains(problems[0], "produce -> branch:fail -> detour -> consume") {
		t.Errorf("problem = %q, want it to name the exact path", problems[0])
	}
}

// TestPathSimulationIsConservativeForUndeclaredProducer keeps the check
// conservative, matching CheckStageContracts: a predecessor that declares no
// expectedOutputs at all is unknown, not wrong.
func TestPathSimulationIsConservativeForUndeclaredProducer(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "produce",
		Tasks: []apiv1.Task{
			{
				Name: "produce", Type: apiv1.TaskDeterministic,
				Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "produce"}},
				Next: "consume",
			},
			{
				Name:       "consume",
				Type:       apiv1.TaskDeterministic,
				Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "consume"}},
				InputsFrom: map[string]string{"anything": "anything"},
			},
		},
	}}
	if problems := CheckPathSimulation(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none when the predecessor declares no expectedOutputs at all", problems)
	}
}

// TestPathSimulationHandlesLoopBackWithoutHanging is the acceptance
// criterion's hard requirement: a gate that routes an outcome back to itself
// must terminate the walk via the (state, live-output-signature) memoization,
// not recurse forever.
func TestPathSimulationHandlesLoopBackWithoutHanging(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "produce",
		Tasks: []apiv1.Task{{
			Name: "produce", Type: apiv1.TaskDeterministic,
			Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "produce"}},
			Next: "loop-gate",
		}},
		Gates: []apiv1.Gate{{
			Name: "loop-gate", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "output-equals", Params: map[string]string{"key": "x", "equals": "y"}},
			Branches:  map[string]string{"pass": TerminalComplete, "fail": "loop-gate"},
		}},
	}}
	done := make(chan []string, 1)
	go func() { done <- CheckPathSimulation(def) }()
	select {
	case problems := <-done:
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none", problems)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CheckPathSimulation did not terminate on a self-routing gate")
	}
}

// TestShippedWorkflowsSatisfyPathSimulation is path simulation's own standing
// guard on the pipeline that builds this repo, mirroring
// TestShippedWorkflowsSatisfyStageContracts (#900) — zero false positives is
// an acceptance criterion for #913, not just a nice-to-have: a check nobody
// can adopt because it fails on its own repo's workflows would be worse than
// no check at all.
func TestShippedWorkflowsSatisfyPathSimulation(t *testing.T) {
	for _, root := range shippedWorkflowRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		var seen int
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			seen++
			path := filepath.Join(root, entry.Name())
			t.Run(filepath.Join(filepath.Base(filepath.Dir(root)), entry.Name()), func(t *testing.T) {
				def := loadWorkflowFile(t, path)
				for _, problem := range CheckPathSimulation(def) {
					t.Errorf("%s", problem)
				}
			})
		}
		if seen == 0 {
			t.Fatalf("no workflow yaml found under %s — this guard is not actually checking anything", root)
		}
	}
}
