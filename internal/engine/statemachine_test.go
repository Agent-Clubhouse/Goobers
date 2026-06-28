package engine

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func linearSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"},
		},
	}
}

func gatedSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          TerminalComplete,
					"fail":          TargetAbort,
					"needs-changes": "implement",
				},
			},
		},
	}
}

func TestCompileValid(t *testing.T) {
	if _, err := Compile(Definition{Name: "linear", Version: 1, Spec: linearSpec()}); err != nil {
		t.Fatalf("linear: %v", err)
	}
	if _, err := Compile(Definition{Name: "gated", Version: 1, Spec: gatedSpec()}); err != nil {
		t.Fatalf("gated: %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	cases := []struct {
		name string
		spec apiv1.WorkflowSpec
		want string
	}{
		{
			name: "empty start",
			spec: apiv1.WorkflowSpec{Start: ""},
			want: "start state is empty",
		},
		{
			name: "dangling start",
			spec: apiv1.WorkflowSpec{Start: "ghost"},
			want: `start state "ghost" is not defined`,
		},
		{
			name: "dangling next",
			spec: apiv1.WorkflowSpec{
				Start: "a",
				Tasks: []apiv1.Task{{Name: "a", Type: apiv1.TaskAgentic, Goal: "g", Next: "ghost"}},
			},
			want: `next state "ghost" is not defined`,
		},
		{
			name: "dangling branch",
			spec: apiv1.WorkflowSpec{
				Start: "g",
				Gates: []apiv1.Gate{{Name: "g", Evaluator: apiv1.EvaluatorAgentic, Branches: map[string]string{"pass": "ghost"}}},
			},
			want: `branch "pass" -> "ghost" is not a defined state`,
		},
		{
			name: "gate without branches",
			spec: apiv1.WorkflowSpec{
				Start: "g",
				Gates: []apiv1.Gate{{Name: "g", Evaluator: apiv1.EvaluatorAgentic, Branches: map[string]string{}}},
			},
			want: `gate "g" has no branches`,
		},
		{
			name: "duplicate state",
			spec: apiv1.WorkflowSpec{
				Start: "a",
				Tasks: []apiv1.Task{{Name: "a", Type: apiv1.TaskAgentic, Goal: "g"}},
				Gates: []apiv1.Gate{{Name: "a", Evaluator: apiv1.EvaluatorAgentic, Branches: map[string]string{"pass": ""}}},
			},
			want: `duplicate state "a"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(Definition{Name: "x", Version: 1, Spec: tc.spec})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestBranchTarget(t *testing.T) {
	g := apiv1.Gate{Branches: map[string]string{"pass": "next", "fail": TargetAbort}}
	if target, ok := BranchTarget(g, "pass"); !ok || target != "next" {
		t.Errorf("pass -> %q,%v; want next,true", target, ok)
	}
	if target, ok := BranchTarget(g, "fail"); !ok || target != TargetAbort {
		t.Errorf("fail -> %q,%v; want @abort,true", target, ok)
	}
	if _, ok := BranchTarget(g, "unknown"); ok {
		t.Error("unknown outcome should not resolve to a branch")
	}
}

func TestIsReservedTarget(t *testing.T) {
	if !IsReservedTarget(TargetAbort) || !IsReservedTarget(TargetEscalate) {
		t.Error("abort/escalate should be reserved")
	}
	if IsReservedTarget("") || IsReservedTarget("some-state") {
		t.Error("empty/state names are not reserved")
	}
}
