package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// overlapReviewSpec models the #5592 merge-review shape:
// gather-sibling-context -> overlap-gate, whose overlap outcome is routed to
// overlapTarget while the no-overlap outcome continues into review.
func overlapReviewSpec(check string, params map[string]string, overlapOutcome, overlapTarget string) apiv1.WorkflowSpec {
	noOverlapOutcome := "pass"
	if overlapOutcome == "pass" {
		noOverlapOutcome = "fail"
	}
	goobersTask := func(name, command, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run: &apiv1.DeterministicRun{Command: []string{"goobers", command}}, Next: next,
		}
	}
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "gather-sibling-context",
		Tasks: []apiv1.Task{
			goobersTask("gather-sibling-context", "gather-sibling-context", "overlap-gate"),
			goobersTask("elect-lander", "elect-lander", "elect-gate"),
			goobersTask("apply-verdict", "apply-verdict", ""),
			{Name: "notify", Type: apiv1.TaskDeterministic, Goal: "notify", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
		Gates: []apiv1.Gate{
			{
				Name: "overlap-gate", Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: check, Params: params},
				Branches:  map[string]string{noOverlapOutcome: "review", overlapOutcome: overlapTarget},
			},
			{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{"pass": "apply-verdict", "needs-changes": "elect-lander", "fail": "apply-verdict"},
			},
			{
				Name: "elect-gate", Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "output-equals", Params: map[string]string{"key": "elected", "equals": "true"}},
				Branches:  map[string]string{"pass": "apply-verdict", "fail": "apply-verdict"},
			},
			{
				Name: "silent-review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{"pass": "notify", "needs-changes": "", "fail": "@abort", BranchEscalate: "apply-verdict"},
			},
		},
	}
}

func TestCheckSiblingOverlapSequencing(t *testing.T) {
	notOverlapping := map[string]string{"key": "hasSiblingOverlap", "equals": "false"}
	overlapping := map[string]string{"key": "hasSiblingOverlap", "equals": "true"}
	tests := []struct {
		name     string
		check    string
		params   map[string]string
		outcome  string
		target   string
		wantWarn bool
	}{
		// The live #5592 graph: overlap-gate(fail: "") terminated every
		// overlapping run without an election or remediation marker.
		{name: "overlap fail terminates", check: "output-equals", params: notOverlapping, outcome: "fail", target: "", wantWarn: true},
		{name: "overlap pass terminates", check: "output-equals", params: overlapping, outcome: "pass", target: "", wantWarn: true},
		{name: "overlap aborts", check: "output-equals", params: notOverlapping, outcome: "fail", target: "@abort", wantWarn: true},
		{name: "not-equals overlap terminates", check: "output-not-equals", params: notOverlapping, outcome: "pass", target: "", wantWarn: true},
		{name: "matches overlap terminates", check: "output-matches", params: map[string]string{"key": "hasSiblingOverlap", "pattern": "^true$"}, outcome: "pass", target: "", wantWarn: true},
		// Neither a non-sequencing task nor a review whose only path to
		// apply-verdict is a runner-forced escalation records state.
		{name: "overlap through unknown task terminates", check: "output-equals", params: notOverlapping, outcome: "fail", target: "notify", wantWarn: true},
		{name: "overlap through silent review terminates", check: "output-equals", params: notOverlapping, outcome: "fail", target: "silent-review", wantWarn: true},
		{name: "overlap routed through elect-lander", check: "output-equals", params: notOverlapping, outcome: "fail", target: "elect-lander"},
		{name: "overlap routed through review", check: "output-equals", params: notOverlapping, outcome: "fail", target: "review"},
		{name: "overlap routed to apply-verdict", check: "output-equals", params: overlapping, outcome: "pass", target: "apply-verdict"},
		{name: "overlap escalates to an operator", check: "output-equals", params: notOverlapping, outcome: "fail", target: "@escalate"},
		{name: "unrelated key is ignored", check: "output-equals", params: map[string]string{"key": "scopeGateParked", "equals": "false"}, outcome: "fail", target: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := Definition{Name: "merge-review", Version: 1, Spec: overlapReviewSpec(tt.check, tt.params, tt.outcome, tt.target)}
			problems := CheckSiblingOverlapSequencing(def)
			if !tt.wantWarn {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want exactly one", problems)
			}
			for _, want := range []string{`gate "overlap-gate"`, "hasSiblingOverlap=true", "elect-lander", "apply-verdict", "indefinitely"} {
				if !strings.Contains(problems[0], want) {
					t.Errorf("problem = %q, want it to contain %q", problems[0], want)
				}
			}
		})
	}
}

// TestShippedMergeReviewWorkflowsSequenceSiblingOverlap holds every shipped
// merge-review graph to the #5592 rule.
func TestShippedMergeReviewWorkflowsSequenceSiblingOverlap(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "merge-review.yaml"),
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web", "workflows", "merge-review.yaml"),
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web-claude", "workflows", "merge-review.yaml"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var w apiv1.Workflow
		if err := yaml.Unmarshal(raw, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		if problems := CheckSiblingOverlapSequencing(Definition{Name: w.Name, Version: 1, Spec: w.Spec}); len(problems) != 0 {
			t.Errorf("%s: problems = %v, want none", path, problems)
		}
	}
}
