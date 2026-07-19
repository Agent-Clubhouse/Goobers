package workflow

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
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

func TestCompileRejectsHumanGate(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "approval",
		Gates: []apiv1.Gate{{
			Name:      "approval",
			Evaluator: apiv1.EvaluatorHuman,
			Human:     &apiv1.HumanGate{Approvers: []string{"maintainers"}},
			Branches:  map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
		}},
	}

	_, err := Compile(Definition{Name: "human-approval", Version: 1, Spec: spec})
	const want = "human gates ship with durable pause/resume (#168/#465); until then use an automated gate or remove this block"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected actionable human-gate rejection, got %v", err)
	}
}

func TestCompileOnTimeoutPolicy(t *testing.T) {
	base := func(taskType apiv1.TaskType, onTimeout string) Definition {
		task := apiv1.Task{Name: "implement", Type: taskType, Goal: "do work", OnTimeout: onTimeout, Next: TerminalComplete}
		if taskType == apiv1.TaskAgentic {
			task.Goober = "coder"
		} else {
			task.Run = &apiv1.DeterministicRun{Command: []string{"true"}}
		}
		return Definition{Name: "ot", Version: 1, Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
			Start:    "implement",
			Tasks:    []apiv1.Task{task},
		}}
	}
	cases := []struct {
		name    string
		def     Definition
		wantErr string
	}{
		{name: "agentic salvage ok", def: base(apiv1.TaskAgentic, apiv1.TaskOnTimeoutSalvage)},
		{name: "agentic fail ok", def: base(apiv1.TaskAgentic, apiv1.TaskOnTimeoutFail)},
		{name: "empty ok", def: base(apiv1.TaskAgentic, "")},
		{name: "unknown value", def: base(apiv1.TaskAgentic, "retry"), wantErr: `onTimeout "retry" is not one of fail, salvage`},
		{name: "salvage on deterministic", def: base(apiv1.TaskDeterministic, apiv1.TaskOnTimeoutSalvage), wantErr: "onTimeout=salvage requires an agentic task"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.def)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Compile: unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Compile error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckWarningsBacklogClaimRequiresResultFile(t *testing.T) {
	task := apiv1.Task{
		Name: "query-backlog",
		Type: apiv1.TaskDeterministic,
		Goal: "claim one item",
		Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}},
	}
	cases := []struct {
		name     string
		command  []string
		inputs   map[string]string
		wantWarn bool
	}{
		{name: "missing result file", command: task.Run.Command, wantWarn: true},
		{name: "empty result file", command: task.Run.Command, inputs: map[string]string{"resultFile": "  "}, wantWarn: true},
		{name: "configured result file", command: task.Run.Command, inputs: map[string]string{"resultFile": "claimed-item.json"}},
		{name: "read only query", command: []string{"goobers", "backlog-query"}},
		{name: "unrelated claim flag", command: []string{"goobers", "pr-select", "--claim"}},
		{name: "shell command", command: []string{"sh", "-c", "goobers backlog-query --claim"}, wantWarn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle:   "web",
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
				Start:    task.Name,
				Tasks:    []apiv1.Task{task},
			}
			spec.Tasks[0].Run = &apiv1.DeterministicRun{Command: tc.command}
			spec.Tasks[0].Inputs = tc.inputs
			def := Definition{Name: "claim", Version: 1, Spec: spec}

			if _, err := Compile(def); err != nil {
				t.Fatalf("warning must not fail compilation: %v", err)
			}
			warnings := CheckWarnings(def)
			if tc.wantWarn {
				if len(warnings) != 1 || !strings.Contains(warnings[0], `task "query-backlog"`) ||
					!strings.Contains(warnings[0], "inputs.resultFile") {
					t.Fatalf("warnings = %v, want one actionable resultFile warning", warnings)
				}
			} else if len(warnings) != 0 {
				t.Fatalf("warnings = %v, want none", warnings)
			}
		})
	}
}

func TestCheckWarningsAcceptedButInertFields(t *testing.T) {
	def := Definition{Name: "inert-fields", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "build",
		Tasks: []apiv1.Task{{
			Name:            "build",
			Type:            apiv1.TaskDeterministic,
			Goal:            "build",
			Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Image: "alpine:3.20"},
			ExpectedOutputs: []string{"artifact"},
		}},
	}}

	if _, err := Compile(def); err != nil {
		t.Fatalf("warnings must not fail compilation: %v", err)
	}
	warnings := CheckWarnings(def)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want expectedOutputs and run.image warnings", warnings)
	}
	all := strings.Join(warnings, "\n")
	for _, want := range []string{
		"expectedOutputs is declared but the stage has no inputs.resultFile to emit it through",
		"run.image is not honored by the local runner",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings = %v, want warning containing %q", warnings, want)
		}
	}
}

func TestCompileManualOnlyTrigger(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerManual}}
	if _, err := Compile(Definition{Name: "manual", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("manual-only workflow should compile, got %v", err)
	}

	spec.Triggers = append(spec.Triggers, apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "@daily"})
	_, err := Compile(Definition{Name: "mixed", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "type=manual must be the only trigger") {
		t.Fatalf("manual trigger mixed with an automatic trigger should fail, got %v", err)
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

// TestCompileAllowsMultipleScheduleTriggers is #341's compile-time half:
// issue #142 originally made a second schedule trigger a hard compile error
// because the runtime scheduler at the time only ever honored the first one.
// #341 gave the runtime real multi-schedule support (Scheduler.Tick fires if
// any of a workflow's schedules is due), so a workflow declaring more than
// one schedule trigger must compile clean now, not fail.
func TestCompileAllowsMultipleScheduleTriggers(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{
		{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"},
		{Type: apiv1.TriggerSchedule, Schedule: "0 9 * * *"},
	}
	if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("multiple schedule triggers should compile clean, got %v", err)
	}
}

// TestCompileRejectsMalformedScheduleAmongMultiple proves each schedule
// trigger is still validated individually even when there's more than one —
// #341 removed the multiplicity rejection, not the per-expression check.
func TestCompileRejectsMalformedScheduleAmongMultiple(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{
		{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"},
		{Type: apiv1.TriggerSchedule, Schedule: "not-a-cron-expression"},
	}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "invalid schedule") {
		t.Fatalf("expected an invalid-schedule error, got %v", err)
	}
}

// TestCompileRejectsSignalTriggerWithNoName is the regression test for
// #125's trigger cross-field validation: a type=signal trigger with no
// Signal name has nothing to fire on, but previously passed schema and
// compiler unnoticed.
func TestCompileRejectsSignalTriggerWithNoName(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSignal}}
	_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `trigger[0] type=signal requires a signal name`) {
		t.Fatalf("expected missing-signal-name error, got %v", err)
	}

	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSignal, Signal: "upstream-workflow-done"}}
	if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("a named signal trigger should compile, got %v", err)
	}
}

func TestCompileRejectsUnknownWorkspace(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0] = apiv1.Task{
		Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
		Run: &apiv1.DeterministicRun{
			Command:   []string{"true"},
			Workspace: apiv1.WorkspaceMode("host"),
		},
	}
	_, err := Compile(Definition{Name: "bad-workspace", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `unknown workspace "host"`) {
		t.Fatalf("Compile error = %v, want unknown workspace", err)
	}
}

func TestCompileRejectsUnknownNetworkMode(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0] = apiv1.Task{
		Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
		Run: &apiv1.DeterministicRun{
			Command: []string{"true"},
			Network: apiv1.NetworkMode("host"),
		},
	}
	_, err := Compile(Definition{Name: "bad-network", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `unknown network mode "host"`) {
		t.Fatalf("Compile error = %v, want unknown network mode", err)
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
	if _, err := Compile(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}),
	); err != nil {
		t.Fatalf("granted capabilities should compile, got %v", err)
	}

	// Drop repo:push from the grant set -> admission fails closed.
	goobers["coder"] = apiv1.GooberSpec{Role: "coder", Harness: apiv1.HarnessCopilot, Capabilities: []string{"github:issues:write"}}
	_, err := Compile(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}),
	)
	if err == nil || !strings.Contains(err.Error(), `uses capability "repo:push" not granted to goober "coder"`) {
		t.Fatalf("expected undeclared-capability error, got %v", err)
	}
}

func TestCompileCIPollRequiresGitHubPRWrite(t *testing.T) {
	cases := []struct {
		name    string
		caps    []string
		wantErr string
	}{
		{
			name:    "missing required capability",
			wantErr: `task "poll" with inputs.kind="ci-poll" must declare capability "github:pr:write"`,
		},
		{
			name: "required capability declared",
			caps: []string{string(capability.GitHubPRWrite)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web",
				Start:  "poll",
				Tasks: []apiv1.Task{{
					Name:         "poll",
					Type:         apiv1.TaskDeterministic,
					Goal:         "poll CI",
					Run:          &apiv1.DeterministicRun{Command: []string{"true"}},
					Inputs:       map[string]string{"kind": "ci-poll"},
					Capabilities: tc.caps,
				}},
			}
			_, err := Compile(Definition{Name: "ci-poll", Version: 1, Spec: spec})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Compile: unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Compile error = %v, want containing %q", err, tc.wantErr)
			}
		})
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
				Branches:  map[string]string{"pass": TerminalComplete, "fail": "poll", "timeout": TargetEscalate},
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

func TestCompileAcceptsNewAutomatedCheckParams(t *testing.T) {
	cases := []struct {
		check  string
		params map[string]string
	}{
		{"output-numeric-lte", map[string]string{"key": "changedFiles", "threshold": "50"}},
		{"output-numeric-lt", map[string]string{"key": "warnings", "threshold": "3"}},
		{"output-not-equals", map[string]string{"key": "status", "equals": "skipped"}},
		{"output-matches", map[string]string{"key": "branch", "pattern": `^release/v\d+$`}},
	}
	for _, tc := range cases {
		t.Run(tc.check, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web",
				Start:  "gate-only",
				Gates: []apiv1.Gate{{
					Name:      "gate-only",
					Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: tc.check, Params: tc.params},
					Branches:  map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
				}},
			}
			if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
				t.Fatalf("Compile: unexpected error %v", err)
			}
		})
	}
}

func TestCompileRejectsInvalidAutomatedCheckParams(t *testing.T) {
	cases := []struct {
		name    string
		check   string
		params  map[string]string
		wantErr string
	}{
		{"gte non-numeric threshold", "output-numeric-gte", map[string]string{"key": "coverage", "threshold": "high"}, `params.threshold "high" is not numeric`},
		{"lte missing key", "output-numeric-lte", map[string]string{"threshold": "50"}, "requires params.key"},
		{"lte missing threshold", "output-numeric-lte", map[string]string{"key": "changedFiles"}, "requires params.threshold"},
		{"lt non-numeric threshold", "output-numeric-lt", map[string]string{"key": "warnings", "threshold": "few"}, `params.threshold "few" is not numeric`},
		{"not-equals missing key", "output-not-equals", map[string]string{"equals": "skipped"}, "requires params.key"},
		{"not-equals missing equals", "output-not-equals", map[string]string{"key": "status"}, "requires params.equals"},
		{"matches missing key", "output-matches", map[string]string{"pattern": `.*`}, "requires params.key"},
		{"matches missing pattern", "output-matches", map[string]string{"key": "branch"}, "requires params.pattern"},
		{"matches invalid pattern", "output-matches", map[string]string{"key": "branch", "pattern": `(`}, "is not a valid RE2 expression"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web",
				Start:  "gate-only",
				Gates: []apiv1.Gate{{
					Name:      "gate-only",
					Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: tc.check, Params: tc.params},
					Branches:  map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
				}},
			}
			_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Compile error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCompileAdmissionUnknownCapabilityGranted(t *testing.T) {
	spec := linearSpec()
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.HarnessCopilot, Capabilities: []string{"github:prs:write"}},
	}
	_, err := Compile(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}),
	)
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
	_, err := Compile(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}),
	)
	if err == nil || !strings.Contains(err.Error(), `task "implement" declares unknown capability "github:pulls:write"`) {
		t.Fatalf("expected unknown-capability-declared error, got %v", err)
	}
}

func TestCompileAdmissionUnknownHarness(t *testing.T) {
	spec := linearSpec()
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.Harness("nonesuch")},
	}
	_, err := Compile(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}),
	)
	if err == nil || !strings.Contains(err.Error(), `unknown harness "nonesuch"`) {
		t.Fatalf("expected unknown-harness error, got %v", err)
	}
}

func TestCompileAdmissionUsesRegisteredHarnessNames(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.Harness("alternate")},
	}
	def := Definition{Name: "x", Version: 1, Spec: linearSpec()}

	if _, err := Compile(def, WithGoobers(goobers), WithKnownHarnesses([]string{"alternate"})); err != nil {
		t.Fatalf("registered harness should compile, got %v", err)
	}
	if _, err := Compile(def, WithGoobers(goobers), WithKnownHarnesses(nil)); err == nil ||
		!strings.Contains(err.Error(), `unknown harness "alternate"`) {
		t.Fatalf("unregistered harness should fail closed, got %v", err)
	}
}

// TestCompileDeterministicTaskUnknownCapability is the regression test for
// #124's deterministic-task admission gap: capability admission previously
// skipped every deterministic task entirely (`t.Type != apiv1.TaskAgentic`
// short-circuited the whole loop body, including the canonical-registry
// check that doesn't need a goober at all), so a typo'd capability on a
// deterministic task passed compilation and surfaced only as a silent
// no-credential failure mid-run.
func TestCompileDeterministicTaskUnknownCapability(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "build",
		Tasks: []apiv1.Task{
			{Name: "build", Type: apiv1.TaskDeterministic, Goal: "g",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Capabilities: []string{"github:pr:wirte"}},
		},
	}
	// WithGoobers supplied (even though this task has none) — matches the
	// real config-validation call site (api/validate's CheckAdmission always
	// passes the full goober set), so this must fail with goobers present.
	_, err := Compile(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(map[string]apiv1.GooberSpec{}),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}),
	)
	if err == nil || !strings.Contains(err.Error(), `task "build" declares unknown capability "github:pr:wirte"`) {
		t.Fatalf("expected unknown-capability error for the deterministic task, got %v", err)
	}
}

// TestCompileGateOutcomeCoverage is the regression test for #124's first
// defect class: a gate branch that can never be taken (not a producible
// outcome), and a producible outcome with no branch to send it to (today
// only failing at evaluation time, internal/gate/evaluate.go's "outcome has
// no defined branch").
func TestCompileGateOutcomeCoverage(t *testing.T) {
	agenticGate := func(branches map[string]string) apiv1.WorkflowSpec {
		return apiv1.WorkflowSpec{
			Gaggle: "web",
			Start:  "implement",
			Tasks:  []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "g", Next: "review"}},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: branches,
			}},
		}
	}

	t.Run("unproducible branch key", func(t *testing.T) {
		spec := agenticGate(map[string]string{"pass": TerminalComplete, "fail": TargetAbort, "needs-changes": "implement", "reject": TargetAbort})
		_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
		if err == nil || !strings.Contains(err.Error(), `gate "review": branch "reject" is not a producible outcome`) {
			t.Fatalf("expected unproducible-branch error, got %v", err)
		}
	})

	t.Run("missing producible outcome", func(t *testing.T) {
		spec := agenticGate(map[string]string{"pass": TerminalComplete, "fail": TargetAbort}) // no needs-changes
		_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
		if err == nil || !strings.Contains(err.Error(), `gate "review": producible outcome "needs-changes" has no branch`) {
			t.Fatalf("expected missing-outcome error, got %v", err)
		}
	})

	t.Run("full coverage compiles", func(t *testing.T) {
		spec := agenticGate(map[string]string{"pass": TerminalComplete, "fail": TargetAbort, "needs-changes": "implement"})
		if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
			t.Fatalf("full outcome coverage should compile, got %v", err)
		}
	})

	t.Run("escalation control branch compiles", func(t *testing.T) {
		spec := agenticGate(map[string]string{
			"pass": TerminalComplete, "fail": TargetAbort, "needs-changes": "implement",
			BranchEscalate: TargetAbort,
		})
		if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
			t.Fatalf("escalation control branch should compile, got %v", err)
		}
	})

	t.Run("automated gate missing fail branch", func(t *testing.T) {
		spec := apiv1.WorkflowSpec{
			Gaggle: "web",
			Start:  "gate-only",
			Tasks:  []apiv1.Task{{Name: "sink", Type: apiv1.TaskDeterministic, Goal: "g", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
			Gates: []apiv1.Gate{{
				Name: "gate-only", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{"pass": "sink"},
			}},
		}
		_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
		if err == nil || !strings.Contains(err.Error(), `gate "gate-only": producible outcome "fail" has no branch`) {
			t.Fatalf("expected missing-fail-branch error, got %v", err)
		}
	})

	// #758's merge-policy abstraction: "land-outcome" (merged/enqueued/fail)
	// and "queue-outcome" (merged/evicted/timeout/fail) get the same
	// compile-time coverage guarantee ci-status's timeout outcome already
	// has — a workflow definition missing a branch for any producible
	// outcome fails Compile, not just the first live run that reaches it.
	t.Run("land-outcome missing enqueued branch", func(t *testing.T) {
		spec := apiv1.WorkflowSpec{
			Gaggle: "web",
			Start:  "gate-only",
			Tasks:  []apiv1.Task{{Name: "sink", Type: apiv1.TaskDeterministic, Goal: "g", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
			Gates: []apiv1.Gate{{
				Name: "gate-only", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "land-outcome"},
				Branches: map[string]string{"merged": "sink", "fail": "sink"},
			}},
		}
		_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
		if err == nil || !strings.Contains(err.Error(), `gate "gate-only": producible outcome "enqueued" has no branch`) {
			t.Fatalf("expected missing-enqueued-branch error, got %v", err)
		}
	})

	t.Run("queue-outcome full coverage compiles", func(t *testing.T) {
		spec := apiv1.WorkflowSpec{
			Gaggle: "web",
			Start:  "gate-only",
			Tasks:  []apiv1.Task{{Name: "sink", Type: apiv1.TaskDeterministic, Goal: "g", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
			Gates: []apiv1.Gate{{
				Name: "gate-only", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "queue-outcome"},
				Branches: map[string]string{"merged": "sink", "evicted": "sink", "timeout": "", "fail": ""},
			}},
		}
		if _, err := Compile(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
			t.Fatalf("full queue-outcome coverage should compile, got %v", err)
		}
	})

	t.Run("queue-outcome missing evicted branch", func(t *testing.T) {
		spec := apiv1.WorkflowSpec{
			Gaggle: "web",
			Start:  "gate-only",
			Tasks:  []apiv1.Task{{Name: "sink", Type: apiv1.TaskDeterministic, Goal: "g", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
			Gates: []apiv1.Gate{{
				Name: "gate-only", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "queue-outcome"},
				Branches: map[string]string{"merged": "sink", "timeout": "", "fail": ""},
			}},
		}
		_, err := Compile(Definition{Name: "x", Version: 1, Spec: spec})
		if err == nil || !strings.Contains(err.Error(), `gate "gate-only": producible outcome "evicted" has no branch`) {
			t.Fatalf("expected missing-evicted-branch error, got %v", err)
		}
	})
}

// TestCompileWithKnownChecksRejectsUnknownCheckName is the regression test
// for #124's second defect class: nothing validated AutomatedGate.Check
// against the actual registry, so a typo'd check name compiled clean and
// only errored once a run actually reached that gate
// (internal/gate/automated.go's "unknown automated check").
func TestCompileWithKnownChecksRejectsUnknownCheckName(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "gate-only",
		Tasks:  []apiv1.Task{{Name: "sink", Type: apiv1.TaskDeterministic, Goal: "g", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
		Gates: []apiv1.Gate{{
			Name: "gate-only", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "ci-green"},
			Branches: map[string]string{"pass": "sink", "fail": "sink"},
		}},
	}
	def := Definition{Name: "x", Version: 1, Spec: spec}

	_, err := Compile(def, WithKnownChecks([]string{"status-equals", "ci-status"}))
	if err == nil || !strings.Contains(err.Error(), `gate "gate-only": unknown automated check "ci-green"`) {
		t.Fatalf("expected unknown-check error, got %v", err)
	}

	// Without WithKnownChecks (the runner path default), check names are not
	// validated — internal/gate itself still fails closed at evaluation time
	// regardless, per the doc comment on WithKnownChecks.
	if _, err := Compile(def); err != nil {
		t.Fatalf("check-name validation should be opt-in; compiled without WithKnownChecks, got %v", err)
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
