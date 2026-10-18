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
			{
				Name: "mixed-complete-review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{"pass": "apply-verdict", "fail": ""},
			},
			{
				Name: "mixed-abort-review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{"pass": "elect-lander", "fail": "@abort"},
			},
			{
				Name: "visible-review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{"pass": "apply-verdict", "fail": "@escalate"},
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
		// One sequencing outcome does not cover a sibling outcome that still
		// terminates silently (#5592 merge-review finding).
		{name: "overlap through mixed review completes silently", check: "output-equals", params: notOverlapping, outcome: "fail", target: "mixed-complete-review", wantWarn: true},
		{name: "overlap through mixed review aborts silently", check: "output-equals", params: notOverlapping, outcome: "fail", target: "mixed-abort-review", wantWarn: true},
		{name: "overlap review sequences or escalates", check: "output-equals", params: notOverlapping, outcome: "fail", target: "visible-review"},
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

// TestCheckSiblingOverlapSequencingLoopsAndParallels pins the all-paths walk
// through loops and parallel arms, where one sequencing route must not hide a
// silent one.
func TestCheckSiblingOverlapSequencingLoopsAndParallels(t *testing.T) {
	notOverlapping := map[string]string{"key": "hasSiblingOverlap", "equals": "false"}
	reviewGate := func(name string, branches map[string]string) apiv1.Gate {
		return apiv1.Gate{Name: name, Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}, Branches: branches}
	}
	plainTask := func(name, next string) apiv1.Task {
		return apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: name, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: next}
	}
	tests := []struct {
		name      string
		target    string
		tasks     []apiv1.Task
		gates     []apiv1.Gate
		parallels []apiv1.Parallel
		wantWarn  bool
	}{
		{
			name: "loop whose only exit is silent", target: "loop-gate", wantWarn: true,
			tasks: []apiv1.Task{plainTask("loop-task", "loop-gate")},
			gates: []apiv1.Gate{reviewGate("loop-gate", map[string]string{"retry": "loop-task", "done": "@abort"})},
		},
		{
			name: "loop whose exit sequences", target: "loop-gate",
			tasks: []apiv1.Task{plainTask("loop-task", "loop-gate")},
			gates: []apiv1.Gate{reviewGate("loop-gate", map[string]string{"retry": "loop-task", "done": "apply-verdict"})},
		},
		{
			// A silent loop reached first through one arm must not be
			// remembered as sequencing when reached again through another.
			name: "parallel arms share a silent loop", target: "par", wantWarn: true,
			tasks: []apiv1.Task{plainTask("loop-task", "loop-gate"), plainTask("join", "")},
			gates: []apiv1.Gate{reviewGate("loop-gate", map[string]string{"retry": "loop-task", "done": "@abort"})},
			parallels: []apiv1.Parallel{{
				Name: "par", FailurePolicy: apiv1.BranchContinueOnError, Join: "join",
				Branches: []apiv1.Branch{{Name: "a", Start: "loop-gate"}, {Name: "b", Start: "loop-task"}},
			}},
		},
		{
			name: "all_or_nothing arm sequences", target: "par",
			tasks: []apiv1.Task{plainTask("join", ""), plainTask("other", "@join"), {Name: "arm-verdict", Type: apiv1.TaskDeterministic, Goal: "v", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "apply-verdict"}}, Next: "@join"}},
			parallels: []apiv1.Parallel{{
				Name: "par", FailurePolicy: apiv1.BranchAllOrNothing, Join: "join", OnFailure: "@abort",
				Branches: []apiv1.Branch{{Name: "a", Start: "other"}, {Name: "b", Start: "arm-verdict"}},
			}},
		},
		{
			// An arm that aborts ends the run before a later arm sequences.
			name: "continue_on_error arm aborts beside sequencing arm", target: "par", wantWarn: true,
			tasks: []apiv1.Task{plainTask("join", ""), {Name: "arm-verdict", Type: apiv1.TaskDeterministic, Goal: "v", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "apply-verdict"}}, Next: "@join"}},
			gates: []apiv1.Gate{reviewGate("arm-gate", map[string]string{"pass": "@join", "fail": "@abort"})},
			parallels: []apiv1.Parallel{{
				Name: "par", FailurePolicy: apiv1.BranchContinueOnError, Join: "join",
				Branches: []apiv1.Branch{{Name: "a", Start: "arm-gate"}, {Name: "b", Start: "arm-verdict"}},
			}},
		},
		{
			name: "fail_fast arm sequences but failure route is silent", target: "par", wantWarn: true,
			tasks: []apiv1.Task{plainTask("join", ""), {Name: "arm-verdict", Type: apiv1.TaskDeterministic, Goal: "v", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "apply-verdict"}}, Next: "@join"}},
			gates: []apiv1.Gate{reviewGate("arm-gate", map[string]string{"pass": "@join", "fail": "@abort"})},
			parallels: []apiv1.Parallel{{
				Name: "par", FailurePolicy: apiv1.BranchFailFast, Join: "join", OnFailure: "@abort",
				Branches: []apiv1.Branch{{Name: "a", Start: "arm-gate"}, {Name: "b", Start: "arm-verdict"}},
			}},
		},
		{
			name: "arm reaching join unsequenced with silent join", target: "par", wantWarn: true,
			tasks: []apiv1.Task{plainTask("join", ""), plainTask("other", "@join"), plainTask("other2", "@join")},
			parallels: []apiv1.Parallel{{
				Name: "par", FailurePolicy: apiv1.BranchContinueOnError, Join: "join",
				Branches: []apiv1.Branch{{Name: "a", Start: "other"}, {Name: "b", Start: "other2"}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := overlapReviewSpec("output-equals", notOverlapping, "fail", tt.target)
			spec.Tasks = append(spec.Tasks, tt.tasks...)
			spec.Gates = append(spec.Gates, tt.gates...)
			spec.Parallels = append(spec.Parallels, tt.parallels...)
			problems := CheckSiblingOverlapSequencing(Definition{Name: "merge-review", Version: 1, Spec: spec})
			if got := len(problems) == 1; got != tt.wantWarn || len(problems) > 1 {
				t.Fatalf("problems = %v, want warning = %v", problems, tt.wantWarn)
			}
		})
	}
}

// TestShippedWorkflowsSequenceSiblingOverlap holds every shipped workflow
// graph to the #5592 rule, so a hasSiblingOverlap gate added to any of them is
// checked.
func TestShippedWorkflowsSequenceSiblingOverlap(t *testing.T) {
	var paths []string
	for _, pattern := range []string{
		filepath.Join("..", "..", "reference-workflows", "gaggles", "*", "workflows", "*.yaml"),
		filepath.Join("..", "..", "config-examples", "gaggles", "*", "workflows", "*.yaml"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		paths = append(paths, matches...)
	}
	if len(paths) == 0 {
		t.Fatal("found no shipped workflows")
	}
	for _, path := range paths {
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
