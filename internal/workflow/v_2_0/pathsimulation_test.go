package v20

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
// regression, matching v_current's own pin.
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

// TestPathSimulationClearsOnceResultFileDeclared is the actual #900 fix.
func TestPathSimulationClearsOnceResultFileDeclared(t *testing.T) {
	if problems := CheckPathSimulation(electLanderIncidentFixture(true)); len(problems) != 0 {
		t.Fatalf("problems = %v, want none once resultFile is declared", problems)
	}
}

// TestPathSimulationIsConservativeForUndeclaredProducer keeps the check
// conservative, matching CheckStageContracts.
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
// must terminate via the (state, live-output-signature) memoization.
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

// TestPathSimulationDescendsIntoParallelBranches is v_2_0's own axis: a bare
// inputsFrom reference inside a parallel branch must be checked against
// whatever live output the state BEFORE the parallel carried in — v_current
// has no Parallel construct at all, so this is the one shape only v_2_0's
// walk needs to handle.
func TestPathSimulationDescendsIntoParallelBranches(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[0].Inputs = map[string]string{"resultFile": "r.json"}
	def.Spec.Tasks[0].ExpectedOutputs = []string{"digest"}
	def.Spec.Tasks[1].InputsFrom = map[string]string{"digest": "digest"}   // review-security: resolves
	def.Spec.Tasks[2].InputsFrom = map[string]string{"missing": "missing"} // review-perf: does not

	problems := CheckPathSimulation(def)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one (review-perf only)", problems)
	}
	if !strings.Contains(problems[0], "review-perf") || !strings.Contains(problems[0], "fan:perf") {
		t.Errorf("problem = %q, want it to name review-perf and the fan:perf branch hop", problems[0])
	}
}

// TestPathSimulationSkipsStageQualifiedReferences confirms the scope
// boundary documented on pathSimulationProblems: a stage-qualified reference
// is owned by qualifiedRefProblems (stagecontract.go), which already answers
// "does the named producer run on every path" — a different and
// already-correct question. Path simulation must stay silent on it rather
// than duplicate or mis-flag it.
func TestPathSimulationSkipsStageQualifiedReferences(t *testing.T) {
	def := parallelDef()
	def.Spec.Tasks[0].Inputs = map[string]string{"resultFile": "r.json"}
	def.Spec.Tasks[0].ExpectedOutputs = []string{"digest"}
	def.Spec.Tasks[2].InputsFrom = map[string]string{"x": "review-security.something"}

	if problems := CheckPathSimulation(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none — stage-qualified references are owned by a different check", problems)
	}
}

// TestShippedWorkflowsSatisfyPathSimulation is path simulation's own standing
// guard on the pipeline that builds this repo, mirroring v_current's own.
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
