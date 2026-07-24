package vnext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestShippedWorkflowsSatisfyRequiredInputs is the standing guard that every
// workflow this repo ships wires every input its `goobers` subcommands
// hard-require. It reuses shippedWorkflowRoots()/loadWorkflowFile() from the
// stage-contract guard.
//
// The defect it prevents is #1061: merge-review's apply-verdict node lost its
// selectedHeadSha/selectedBaseSha wiring in a hand-maintained instance config
// while the binary kept requiring them, so every election crashed and none
// completed for the life of that build — invisible to compile and to
// `goobers validate`. If a change here has to drop a required input it must be
// an explicit edit to stageRequiredInputs, which is exactly the visibility the
// live failure lacked.
func TestShippedWorkflowsSatisfyRequiredInputs(t *testing.T) {
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
				for _, problem := range CheckStageRequiredInputs(def) {
					t.Errorf("%s", problem)
				}
			})
		}
		if seen == 0 {
			t.Fatalf("no workflow yaml found under %s — this guard is not actually checking anything", root)
		}
	}
}

// applyVerdictWorkflow is a minimal, structurally-valid single-stage workflow
// whose only task runs `goobers apply-verdict`. Wiring is set per-test.
func applyVerdictWorkflow(inputs, inputsFrom map[string]string) Definition {
	return Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "apply-verdict",
		Tasks: []apiv1.Task{{
			Name:       "apply-verdict",
			Type:       apiv1.TaskDeterministic,
			Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "apply-verdict"}},
			Inputs:     inputs,
			InputsFrom: inputsFrom,
		}},
	}}
}

// TestMissingRequiredInputIsReported pins the #1061 shape itself: an
// apply-verdict stage that wires selectedNumber but not the SHAs the binary
// hard-requires.
func TestMissingRequiredInputIsReported(t *testing.T) {
	def := applyVerdictWorkflow(nil, map[string]string{"selectedNumber": "number"})
	problems := CheckStageRequiredInputs(def)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", problems)
	}
	for _, want := range []string{"apply-verdict", "selectedHeadSha", "selectedBaseSha"} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("problem %q missing %q", problems[0], want)
		}
	}
	if strings.Contains(problems[0], "selectedNumber") {
		t.Errorf("problem %q should not flag the wired selectedNumber", problems[0])
	}

	// Wiring the two missing SHAs via inputsFrom clears it — the #1061 fix.
	def = applyVerdictWorkflow(nil, map[string]string{
		"selectedNumber":  "number",
		"selectedHeadSha": "selectedHeadSha",
		"selectedBaseSha": "selectedBaseSha",
	})
	if problems := CheckStageRequiredInputs(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none once the SHAs are wired", problems)
	}
}

// TestRequiredInputSatisfiedByStaticInput confirms a static `inputs` value
// counts as wired, not only an inputsFrom edge — merge-pr's `verdict: "pass"`
// is a static input, so treating only inputsFrom as satisfying would
// false-positive on every shipped merge-review.
func TestRequiredInputSatisfiedByStaticInput(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "merge-pr",
		Tasks: []apiv1.Task{{
			Name: "merge-pr",
			Type: apiv1.TaskDeterministic,
			Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "merge-pr"}},
			// verdict static; the rest via inputsFrom.
			Inputs: map[string]string{"verdict": "pass"},
			InputsFrom: map[string]string{
				"pullNumber": "selectedNumber",
				"headSha":    "selectedHeadSha",
				"baseSha":    "selectedBaseSha",
			},
		}},
	}}
	if problems := CheckStageRequiredInputs(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none — verdict is satisfied by a static input", problems)
	}
}

// TestUnknownSubcommandNotFlagged pins the conservatism: a subcommand absent
// from the registry is "requirements unknown" and never reported, so a stale
// registry can only miss a real problem, never invent one.
func TestUnknownSubcommandNotFlagged(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "mystery",
		Tasks: []apiv1.Task{{
			Name: "mystery",
			Type: apiv1.TaskDeterministic,
			Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "some-new-subcommand"}},
		}},
	}}
	if problems := CheckStageRequiredInputs(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none for an unregistered subcommand", problems)
	}
}

// TestNonGoobersCommandNotFlagged confirms a stage whose command is not the
// goobers CLI (e.g. local-ci's `make ci`) is never inspected.
func TestNonGoobersCommandNotFlagged(t *testing.T) {
	def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
		Start: "build",
		Tasks: []apiv1.Task{{
			Name: "build",
			Type: apiv1.TaskDeterministic,
			Run:  &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
		}},
	}}
	if problems := CheckStageRequiredInputs(def); len(problems) != 0 {
		t.Fatalf("problems = %v, want none for a non-goobers command", problems)
	}
}
