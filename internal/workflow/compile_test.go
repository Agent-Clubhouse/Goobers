package workflow

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

func TestCompileStructuralErrors(t *testing.T) {
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

func TestCompileRejectsUnreachableState(t *testing.T) {
	// "orphan" is defined but nothing transitions to it.
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "a",
		Tasks: []apiv1.Task{
			{Name: "a", Type: apiv1.TaskAgentic, Goal: "g"},
			{Name: "orphan", Type: apiv1.TaskAgentic, Goal: "g"},
		},
	}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `state "orphan" is unreachable from start "a"`) {
		t.Fatalf("expected unreachable error, got %v", err)
	}
}

func TestCompileRejectsLoopWithoutExit(t *testing.T) {
	// a -> b -> a: a pure task cycle with no gate exit can never terminate.
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "a",
		Tasks: []apiv1.Task{
			{Name: "a", Type: apiv1.TaskAgentic, Goal: "g", Next: "b"},
			{Name: "b", Type: apiv1.TaskAgentic, Goal: "g", Next: "a"},
		},
	}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "cannot reach a terminal outcome") {
		t.Fatalf("expected loop-without-exit error, got %v", err)
	}
}

func TestCompileAcceptsLoopWithGateExit(t *testing.T) {
	// implement -> review; review can loop back OR pass to terminal. The cycle is
	// fine because the gate provides an exit.
	if _, err := Compile(Definition{Name: "gated", Version: 1, Spec: gatedSpec()}); err != nil {
		t.Fatalf("gate-exited loop should compile, got %v", err)
	}
}

func TestCompileRejectsBadSchedule(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "not a cron"}}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "invalid schedule") {
		t.Fatalf("expected bad-schedule error, got %v", err)
	}
}

func TestValidSchedulesAccepted(t *testing.T) {
	for _, ok := range []string{"0 * * * *", "*/5 0 * * * *", "@daily", "@hourly", "@every 1h30m", "0 0 1 * *"} {
		spec := linearSpec()
		spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: ok}}
		if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
			t.Errorf("schedule %q should be valid, got %v", ok, err)
		}
	}
}

func TestCompileAdmissionCapabilities(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "g",
				Capabilities: []string{"github:issues:write", "repo:push"}},
		},
	}
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.HarnessCopilot, Capabilities: []string{"github:issues:write", "repo:push"}},
	}
	if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}, WithGoobers(goobers)); err != nil {
		t.Fatalf("granted capabilities should compile, got %v", err)
	}

	// Drop repo:push from the grant set -> admission fails closed.
	goobers["coder"] = apiv1.GooberSpec{Role: "coder", Harness: apiv1.HarnessCopilot, Capabilities: []string{"github:issues:write"}}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}, WithGoobers(goobers))
	if err == nil || !strings.Contains(err.Error(), `uses capability "repo:push" not granted to goober "coder"`) {
		t.Fatalf("expected undeclared-capability error, got %v", err)
	}
}

// TestCompileRejectsGateVocabMismatch proves the #132 compile-time check-param
// validation hook: a gate declaring params.equals against the wrong output
// vocabulary for its check now fails Compile instead of compiling clean and
// silently never matching at runtime (the ci-gate bug: ci-poll emits
// providers.CheckState's "passing"/"failing", never apiv1.ResultStatus's
// "success"/"failure").
func TestCompileRejectsGateVocabMismatch(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "poll",
		Tasks: []apiv1.Task{
			{Name: "poll", Type: apiv1.TaskDeterministic, Goal: "poll ci", Next: "ci-gate",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "ci-gate",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "ci-status", Params: map[string]string{"equals": "success"}},
				Branches:  map[string]string{"pass": TerminalComplete, "fail": "poll"},
			},
		},
	}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `check "ci-status" params.equals "success" is not one of`) {
		t.Fatalf("expected a gate-vocabulary-mismatch error, got %v", err)
	}

	// The correct vocabulary for ci-status compiles clean.
	spec.Gates[0].Automated.Params["equals"] = "passing"
	if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("correct ci-status vocabulary should compile, got %v", err)
	}

	// status-equals uses the opposite (apiv1.ResultStatus) vocabulary —
	// "passing" is invalid there too.
	spec.Gates[0].Automated.Check = "status-equals"
	spec.Gates[0].Automated.Params["equals"] = "passing"
	_, err = Compile(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `check "status-equals" params.equals "passing" is not one of`) {
		t.Fatalf("expected a gate-vocabulary-mismatch error for status-equals, got %v", err)
	}
}

func TestCompileAdmissionUnknownCapabilityGranted(t *testing.T) {
	spec := linearSpec()
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.HarnessCopilot, Capabilities: []string{"github:prs:write"}},
	}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}, WithGoobers(goobers))
	if err == nil || !strings.Contains(err.Error(), `goober "coder" grants unknown capability "github:prs:write"`) {
		t.Fatalf("expected unknown-capability-granted error, got %v", err)
	}
}

func TestCompileAdmissionUnknownCapabilityDeclared(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "g",
				Capabilities: []string{"github:pulls:write"}},
		},
	}
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.HarnessCopilot, Capabilities: []string{"github:pulls:write"}},
	}
	// The typo'd spelling is internally consistent (granted == declared), so
	// only the canonical-registry check catches it — the grant-membership
	// check alone would pass this.
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}, WithGoobers(goobers))
	if err == nil || !strings.Contains(err.Error(), `task "implement" declares unknown capability "github:pulls:write"`) {
		t.Fatalf("expected unknown-capability-declared error, got %v", err)
	}
}

func TestCompileAdmissionUnknownHarness(t *testing.T) {
	spec := linearSpec()
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.Harness("nonesuch")},
	}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}, WithGoobers(goobers))
	if err == nil || !strings.Contains(err.Error(), `unknown harness "nonesuch"`) {
		t.Fatalf("expected unknown-harness error, got %v", err)
	}
}

func TestAdmissionSkippedWithoutGoobers(t *testing.T) {
	// Same spec that would fail admission compiles when no goober context is
	// supplied (the runner path — admission already happened at config time).
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "g", Capabilities: []string{"repo:push"}},
		},
	}
	if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("runner path should not run admission, got %v", err)
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
