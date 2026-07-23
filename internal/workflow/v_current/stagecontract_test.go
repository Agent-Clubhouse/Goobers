package vcurrent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// loadWorkflowFile reads one workflow yaml at an explicit path into a
// Definition. Distinct from digest_test.go's loadShippedWorkflow, which
// resolves names against testdata/shipped: these checks run against the
// live config trees the instance and its adopters actually use.
func loadWorkflowFile(t *testing.T, path string) Definition {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return Definition{Name: w.Name, Version: 1, Spec: w.Spec}
}

// shippedWorkflowRoots are every directory of workflow definitions this repo
// ships. selfhost is the config the Goobers instance that builds Goobers
// actually runs, so a regression there breaks our own pipeline; the examples
// are what other instances copy from, so a regression there ships the defect
// to everyone else.
func shippedWorkflowRoots() []string {
	return []string{
		filepath.Join("..", "..", "..", "selfhost", "gaggles", "goobers", "workflows"),
		filepath.Join("..", "..", "..", "config-examples", "gaggles", "acme-web", "workflows"),
	}
}

// TestShippedWorkflowsSatisfyStageContracts is the standing guard on the
// pipeline that builds this repo (#900).
//
// The defect it exists to prevent is silent by construction: merge-review's
// elect-lander declared five expectedOutputs and no resultFile, so it emitted
// none of them while still exiting 0. Its gate read the missing key as false,
// routed every needs-changes review down the wrong branch, and the stage
// after that died resolving an inputsFrom against the same empty outputs.
// Nothing failed until two stages downstream, and the instance sat stalled
// for three days.
//
// Every workflow this repo ships is checked, not just the one that broke: the
// same shape was already present in the work-nomination example, unnoticed.
// If a change here has to break a contract, that is allowed — but it has to
// be an explicit edit to this test, which is exactly the visibility the live
// failure lacked.
func TestShippedWorkflowsSatisfyStageContracts(t *testing.T) {
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
				for _, problem := range CheckStageContracts(def) {
					t.Errorf("%s", problem)
				}
				// Our own pipeline is held to the stricter bar: even the
				// not-yet-breaking form is a failure here, because "nothing
				// reads it yet" is one inputsFrom away from an outage.
				for _, problem := range CheckStageContractWarnings(def) {
					t.Errorf("%s", problem)
				}
			})
		}
		// A root that silently stops matching would turn this whole guard
		// into a no-op that still reports PASS.
		if seen == 0 {
			t.Fatalf("no workflow yaml found under %s — this guard is not actually checking anything", root)
		}
	}
}

// TestUndeclaredResultFileIsReported pins the elect-lander defect itself: a
// shell stage promising outputs with no result file to emit them through.
func TestUndeclaredResultFileIsReported(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "a",
		Tasks: []apiv1.Task{
			{
				Name: "a", Type: apiv1.TaskDeterministic,
				Run:             &apiv1.DeterministicRun{Command: []string{"goobers", "elect-lander"}},
				ExpectedOutputs: []string{"elected"},
				Next:            "b",
			},
			{
				// Reads it, which is what makes the omission breaking
				// rather than merely untidy — the elect-lander shape.
				Name: "b", Type: apiv1.TaskDeterministic,
				Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "b"}},
				InputsFrom: map[string]string{"elected": "elected"},
			},
		},
	}}
	problems := CheckStageContracts(def)
	if len(problems) != 1 || !strings.Contains(problems[0], "no inputs.resultFile") {
		t.Fatalf("problems = %v, want one undeclared-resultFile problem", problems)
	}

	// Declaring one clears it.
	def.Spec.Tasks[0].Inputs = map[string]string{"resultFile": "election.json"}
	if problems := CheckStageContracts(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none once resultFile is declared", problems)
	}
}

// TestNonShellKindsAreExemptFromResultFile guards against the check being
// over-eager: a kind=ci-poll stage is dispatched to CIPollExecutor, which
// never shells out and produces its outputs directly, so it legitimately
// declares expectedOutputs with no result file. Reporting it would have made
// the check un-adoptable on the very workflow it needs to protect.
func TestNonShellKindsAreExemptFromResultFile(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "a",
		Tasks: []apiv1.Task{{
			Name: "a", Type: apiv1.TaskDeterministic,
			Run:             &apiv1.DeterministicRun{Command: []string{"goobers", "ci-poll"}},
			Inputs:          map[string]string{"kind": "ci-poll"},
			ExpectedOutputs: []string{"ciStatus"},
		}},
	}}
	if problems := CheckStageContracts(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none for a non-shell built-in kind", problems)
	}
}

// TestUnsatisfiableInputsFromIsReportedPerBranch is the #562-shaped case:
// a stage reachable by two branches, where only one of the two possible
// preceding stages emits the key it reads. The satisfied branch is exactly
// why this kind of bug survives testing — the happy path works.
func TestUnsatisfiableInputsFromIsReportedPerBranch(t *testing.T) {
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
				// Reached on the other branch, and does NOT re-emit digest.
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
	problems := CheckStageContracts(def)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one (the detour branch only)", problems)
	}
	if !strings.Contains(problems[0], `task "detour"`) || !strings.Contains(problems[0], `"digest"`) {
		t.Fatalf("problem = %q, want it to name the detour path and the digest key", problems[0])
	}
}

// TestInputsFromUndeclaredUpstreamOutputsIsNotReported keeps the check
// conservative. expectedOutputs is not required to be exhaustive — shipped
// workflows deliberately omit conditionally-emitted keys like landOutcome —
// so a predecessor declaring nothing is unknown, not wrong. Reporting it
// would force every workflow to over-declare just to silence the validator.
func TestInputsFromUndeclaredUpstreamOutputsIsNotReported(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "produce",
		Tasks: []apiv1.Task{
			{
				Name: "produce", Type: apiv1.TaskDeterministic,
				Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "produce"}},
				Inputs: map[string]string{"resultFile": "r.json"},
				Next:   "consume",
			},
			{
				Name: "consume", Type: apiv1.TaskDeterministic,
				Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "consume"}},
				InputsFrom: map[string]string{"anything": "anything"},
			},
		},
	}}
	if problems := CheckStageContracts(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none when the predecessor declares no expectedOutputs at all", problems)
	}
}

// TestUnconsumedUndeclaredResultFileIsOnlyAWarning keeps the check adoptable.
// A stage promising outputs nothing reads is untidy, not broken, and
// reporting it as an error would fail every existing config that has one —
// including this repo's own acme-web example, whose query-backlog declares
// claimed-item that no inputsFrom references.
func TestUnconsumedUndeclaredResultFileIsOnlyAWarning(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "a",
		Tasks: []apiv1.Task{{
			Name: "a", Type: apiv1.TaskDeterministic,
			Run:             &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}},
			ExpectedOutputs: []string{"claimed-item"},
		}},
	}}
	if problems := CheckStageContracts(def); len(problems) != 0 {
		t.Fatalf("errors = %v, want none — nothing reads claimed-item", problems)
	}
	warnings := CheckStageContractWarnings(def)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not yet a failure") {
		t.Fatalf("warnings = %v, want one not-yet-a-failure warning", warnings)
	}
}
