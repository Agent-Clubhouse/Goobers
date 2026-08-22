package vcurrent

import (
	"fmt"
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

func compileAcknowledged(def Definition, opts ...Option) (*Machine, error) {
	return Compile(def, append(opts, WithPreviewFeatures(true))...)
}

func TestCompileValid(t *testing.T) {
	if _, err := compileAcknowledged(Definition{Name: "linear", Version: 1, Spec: linearSpec()}); err != nil {
		t.Fatalf("linear: %v", err)
	}
	if _, err := compileAcknowledged(Definition{Name: "gated", Version: 1, Spec: gatedSpec()}); err != nil {
		t.Fatalf("gated: %v", err)
	}
}

func TestCompileRejectsIncompatibleClaimLedgerPlacement(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "claim",
		Tasks: []apiv1.Task{
			{
				Name: "claim", Type: apiv1.TaskDeterministic, Goal: "claim", Next: "release",
				Run:                  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}},
				RequiredCapabilities: []string{"os=linux"},
			},
			{
				Name: "release", Type: apiv1.TaskDeterministic, Goal: "release",
				Run:                  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--release"}},
				RequiredCapabilities: []string{"os=windows"},
			},
		},
	}
	_, err := compileAcknowledged(Definition{Name: "claims-placement", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `claims-mutating tasks "claim" and "release" declare incompatible requiredCapabilities`) {
		t.Fatalf("Compile error = %v, want incompatible claims placement", err)
	}
}

func TestClaimLedgerPlacementProblems(t *testing.T) {
	task := func(name, command string, args, required []string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:                  &apiv1.DeterministicRun{Command: append([]string{"goobers", command}, args...)},
			RequiredCapabilities: required,
		}
	}
	tests := []struct {
		name  string
		tasks []apiv1.Task
		want  bool
	}{
		{
			name: "same placement in different order",
			tasks: []apiv1.Task{
				task("select", "pr-select", nil, []string{"git", "os=linux"}),
				task("release", "pr-claim", []string{"--release"}, []string{"os=linux", "git"}),
			},
		},
		{
			name: "pinned and unpinned differ",
			tasks: []apiv1.Task{
				task("select", "select-source", nil, nil),
				task("close", "issue-close-out", nil, []string{"os=linux"}),
			},
			want: true,
		},
		{
			name: "non-mutating backlog query ignored",
			tasks: []apiv1.Task{
				task("read", "backlog-query", []string{"--read-only"}, []string{"os=windows"}),
				task("claim", "backlog-query", []string{"--claim"}, []string{"os=linux"}),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := claimLedgerPlacementProblems(Definition{Spec: apiv1.WorkflowSpec{Tasks: test.tasks}})
			if (len(got) > 0) != test.want {
				t.Fatalf("claimLedgerPlacementProblems() = %v, want problem %t", got, test.want)
			}
		})
	}
}

func TestCompileRejectsUnknownMinimumIntegrity(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0].MinimumIntegrity = "owner-ish"
	_, err := compileAcknowledged(Definition{Name: "integrity", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `minimumIntegrity "owner-ish"`) {
		t.Fatalf("Compile error = %v, want unknown minimum integrity", err)
	}
}

func TestCompileValidatesBacklogTrustLabel(t *testing.T) {
	t.Run("explicit trust label with routing selectors", func(t *testing.T) {
		spec := linearSpec()
		spec.Triggers = []apiv1.Trigger{{
			Type:       apiv1.TriggerBacklogItem,
			TrustLabel: "team-approved",
			Selector: map[string]string{
				"team-approved": "true",
				"goobers:ready": "true",
			},
		}}
		if _, err := compileAcknowledged(Definition{Name: "trusted", Version: 1, Spec: spec}); err != nil {
			t.Fatalf("Compile: %v", err)
		}
	})

	for _, tc := range []struct {
		name     string
		triggers []apiv1.Trigger
		want     string
	}{
		{
			name:     "blank",
			triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem, TrustLabel: " "}},
			want:     "trustLabel must not be blank",
		},
		{
			name:     "non backlog trigger",
			triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@daily", TrustLabel: "team-approved"}},
			want:     "type=schedule does not support trustLabel",
		},
		{
			name: "conflicting backlog triggers",
			triggers: []apiv1.Trigger{
				{Type: apiv1.TriggerBacklogItem, TrustLabel: "team-approved"},
				{Type: apiv1.TriggerBacklogItem, TrustLabel: "security-approved"},
			},
			want: `trustLabel "security-approved" conflicts with backlog-item trustLabel "team-approved"`,
		},
		{
			name: "mixed declared and omitted trust labels",
			triggers: []apiv1.Trigger{
				{Type: apiv1.TriggerBacklogItem},
				{Type: apiv1.TriggerBacklogItem, TrustLabel: "team-approved"},
			},
			want: `trustLabel "team-approved" conflicts with backlog-item trustLabel ""`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := linearSpec()
			spec.Triggers = tc.triggers
			_, err := compileAcknowledged(Definition{Name: "trusted", Version: 1, Spec: spec})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCompileRejectsUnknownContextSource(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0].ContextFrom = []string{"missing"}
	_, err := compileAcknowledged(Definition{Name: "context-routing", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `contextFrom source "missing"`) {
		t.Fatalf("Compile error = %v, want unknown context source", err)
	}
}

func TestCompileRejectsDuplicateContextSource(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0].ContextFrom = []string{"build", "build"}
	_, err := compileAcknowledged(Definition{Name: "context-routing", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `contextFrom source "build" is duplicated`) {
		t.Fatalf("Compile error = %v, want duplicate context source", err)
	}
}

func TestCompileRejectsRunScriptInFrozenDSL(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		Start:    "check",
		Tasks: []apiv1.Task{{
			Name: "check",
			Type: apiv1.TaskDeterministic,
			Goal: "check",
			Run:  &apiv1.DeterministicRun{Script: "printf 'ok\n'"},
		}},
	}

	_, err := compileAcknowledged(Definition{Name: "inline", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "run.script is not supported in DSL 1.4") {
		t.Fatalf("Compile error = %v, want frozen-DSL run.script rejection", err)
	}
}

func TestCompileAllowsHumanGate(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "approval",
		Gates: []apiv1.Gate{{
			Name:      "approval",
			Evaluator: apiv1.EvaluatorHuman,
			Human:     &apiv1.HumanGate{},
			Branches:  map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
		}},
	}

	if _, err := compileAcknowledged(Definition{Name: "human-approval", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("compile human gate: %v", err)
	}
	spec.Gates[0].Human.Approvers = []string{"maintainers"}
	if _, err := compileAcknowledged(Definition{Name: "restricted-human-approval", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("compile restricted human gate: %v", err)
	}
	spec.Gates[0].Human.TimeoutSeconds = 60
	spec.Gates[0].Human.OnTimeout = "reject"
	if _, err := compileAcknowledged(Definition{Name: "timed-human-approval", Version: 1, Spec: spec}); err == nil ||
		!strings.Contains(err.Error(), "human timeout behavior is not supported yet") {
		t.Fatalf("timed human gate error = %v, want fail-closed timeout rejection", err)
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
			_, err := compileAcknowledged(tc.def)
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
		Name:          "query-backlog",
		Type:          apiv1.TaskDeterministic,
		Goal:          "claim one item",
		Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}},
		Capabilities:  []string{string(capability.GitHubIssuesWrite)},
		PolicyActions: []string{"claim-backlog-items"},
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
		{name: "unrelated claim flag", command: []string{"goobers", "publish-batch", "--claim"}},
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

			if _, err := compileAcknowledged(def); err != nil {
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

func TestCheckWarningsNoScheduleTrigger(t *testing.T) {
	cases := []struct {
		name     string
		triggers []apiv1.Trigger
		want     string
	}{
		{
			name:     "backlog-item-only",
			triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			want:     "workflow \"backlog-item-only\" has no schedule trigger; it will not fire autonomously — run it with `goobers run backlog-item-only`",
		},
		{
			name:     "manual-only",
			triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			want:     "workflow \"manual-only\" has no schedule trigger; it will not fire autonomously — run it with `goobers run manual-only`",
		},
		{
			name:     "scheduled",
			triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		},
		{
			name:     "webhook",
			triggers: []apiv1.Trigger{{Type: apiv1.TriggerWebhook, Events: []string{"issues"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := Definition{Name: tc.name, Spec: apiv1.WorkflowSpec{Triggers: tc.triggers}}
			warnings := CheckWarnings(def)
			if tc.want == "" {
				if len(warnings) != 0 {
					t.Fatalf("warnings = %v, want none", warnings)
				}
				return
			}
			if len(warnings) != 1 || warnings[0] != tc.want {
				t.Fatalf("warnings = %v, want exactly %q", warnings, tc.want)
			}
		})
	}
}

func TestCheckWarningsAcceptedButInertField(t *testing.T) {
	def := Definition{Name: "inert-fields", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		Start:    "build",
		Tasks: []apiv1.Task{{
			Name:            "build",
			Type:            apiv1.TaskDeterministic,
			Goal:            "build",
			Run:             &apiv1.DeterministicRun{Command: []string{"true"}},
			ExpectedOutputs: []string{"artifact"},
		}},
	}}

	if _, err := compileAcknowledged(def); err != nil {
		t.Fatalf("warnings must not fail compilation: %v", err)
	}
	warnings := CheckWarnings(def)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want expectedOutputs warning", warnings)
	}
	want := "expectedOutputs is declared but the stage has no inputs.resultFile to emit it through"
	if !strings.Contains(warnings[0], want) {
		t.Errorf("warnings = %v, want warning containing %q", warnings, want)
	}
}

func TestCompileManualOnlyTrigger(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerManual}}
	if _, err := compileAcknowledged(Definition{Name: "manual", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("manual-only workflow should compile, got %v", err)
	}

	spec.Triggers = append(spec.Triggers, apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "@daily"})
	_, err := compileAcknowledged(Definition{Name: "mixed", Version: 1, Spec: spec})
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
			_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: tc.spec})
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
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
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
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "cannot reach a terminal outcome") {
		t.Fatalf("expected loop-without-exit error, got %v", err)
	}
}

func TestCompileAcceptsLoopWithGateExit(t *testing.T) {
	// implement -> review; review can loop back OR pass to terminal. The cycle is
	// fine because the gate provides an exit.
	if _, err := compileAcknowledged(Definition{Name: "gated", Version: 1, Spec: gatedSpec()}); err != nil {
		t.Fatalf("gate-exited loop should compile, got %v", err)
	}
}

func TestCompileRejectsBadSchedule(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "not a cron"}}
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "invalid schedule") {
		t.Fatalf("expected bad-schedule error, got %v", err)
	}
}

func TestCompileValidatesScheduleIdleBackoff(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{{
		Type:        apiv1.TriggerSchedule,
		Schedule:    "* * * * *",
		IdleBackoff: &apiv1.IdleBackoff{Floor: "10m", Ceiling: "2m"},
	}}
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "ceiling 2m0s must not be below floor 10m0s") {
		t.Fatalf("expected invalid idle-backoff bounds, got %v", err)
	}
}

func TestCompileRejectsScheduleThatNeverFires(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "0 0 30 2 *"}}
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "can never fire") {
		t.Fatalf("expected unsatisfiable-schedule error, got %v", err)
	}
}

func TestValidSchedulesAccepted(t *testing.T) {
	for _, ok := range []string{"0 * * * *", "*/5 0 * * * *", "@daily", "@hourly", "@every 1h30m", "0 0 1 * *"} {
		spec := linearSpec()
		spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: ok}}
		if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
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
	if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
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
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
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
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `trigger[0] type=signal requires a signal name`) {
		t.Fatalf("expected missing-signal-name error, got %v", err)
	}

	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSignal, Signal: "upstream-workflow-done"}}
	if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("a named signal trigger should compile, got %v", err)
	}
}

func TestCompileRejectsWebhookTriggerWithoutEvents(t *testing.T) {
	spec := linearSpec()
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerWebhook}}
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `trigger[0] type=webhook requires at least one event name`) {
		t.Fatalf("expected missing-webhook-events error, got %v", err)
	}

	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerWebhook, Events: []string{"issues", " "}}}
	_, err = compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `trigger[0] type=webhook event[1] must not be empty`) {
		t.Fatalf("expected empty-webhook-event error, got %v", err)
	}

	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerWebhook, Events: []string{"issues", "pull_request"}}}
	if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("a webhook trigger with events should compile, got %v", err)
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
	_, err := compileAcknowledged(Definition{Name: "bad-workspace", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `unknown workspace "host"`) {
		t.Fatalf("Compile error = %v, want unknown workspace", err)
	}
}

func TestCompileRejectsSyncBaseInScratchWorkspace(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0] = apiv1.Task{
		Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
		Run: &apiv1.DeterministicRun{
			Command:   []string{"true"},
			Workspace: apiv1.WorkspaceScratch,
			SyncBase:  true,
		},
	}
	_, err := compileAcknowledged(Definition{Name: "bad-sync-base", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "syncBase requires a repo workspace") {
		t.Fatalf("Compile error = %v, want syncBase repo-workspace requirement", err)
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
	_, err := compileAcknowledged(Definition{Name: "bad-network", Version: 1, Spec: spec})
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
				Capabilities:  []string{"github:issues:write", "repo:push", string(capability.AgentModel)},
				PolicyActions: []string{"label-issue", "modify-repository"}},
		},
	}
	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			Role:          "coder",
			Harness:       apiv1.HarnessCopilot,
			Capabilities:  []string{"github:issues:write", "repo:push", string(capability.AgentModel)},
			PolicyActions: []string{"label-issue", "modify-repository"},
		},
	}
	if _, err := compileAcknowledged(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)})); err != nil {
		t.Fatalf("granted capabilities should compile, got %v", err)
	}

	// Drop repo:push from the grant set -> admission fails closed.
	goobers["coder"] = apiv1.GooberSpec{
		Role:          "coder",
		Harness:       apiv1.HarnessCopilot,
		Capabilities:  []string{"github:issues:write", string(capability.AgentModel)},
		PolicyActions: []string{"label-issue", "modify-repository"},
	}
	_, err := compileAcknowledged(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}))

	if err == nil || !strings.Contains(err.Error(), `uses capability "repo:push" not granted to goober "coder"`) {
		t.Fatalf("expected undeclared-capability error, got %v", err)
	}
}

func TestCompileRequiresTaskMCPCredentialCapabilities(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "g",
			Capabilities: []string{string(capability.AgentModel)},
		}},
	}
	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			Capabilities: []string{"contents:read", string(capability.AgentModel)},
			MCPServers: []apiv1.MCPServer{{
				Name: "context",
				URL:  "https://mcp.example.test",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Capability: "contents:read",
					Header:     "Authorization",
				}},
			}},
		},
	}

	_, err := compileAcknowledged(
		Definition{Name: "mcp-capability", Version: 1, Spec: spec},
		WithGoobers(goobers),
	)
	if err == nil || !strings.Contains(err.Error(), `task "implement" must declare MCP credential capability "contents:read" required by goober "coder"`) {
		t.Fatalf("Compile error = %v, want missing MCP credential capability", err)
	}

	spec.Tasks[0].Capabilities = []string{"contents:read", string(capability.AgentModel)}
	if _, err := compileAcknowledged(
		Definition{Name: "mcp-capability", Version: 1, Spec: spec},
		WithGoobers(goobers),
	); err != nil {
		t.Fatalf("declared MCP credential capability should compile: %v", err)
	}
}

func TestCompileRejectsConfigRepoReadForStagesAndGoobers(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0].Capabilities = []string{string(capability.ConfigRepoRead)}
	_, err := compileAcknowledged(Definition{Name: "runner-only-task", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `task "implement" declares runner-only capability "configrepo:read"`) {
		t.Fatalf("Compile error = %v, want runner-only task capability rejection", err)
	}

	spec = linearSpec()
	_, err = compileAcknowledged(
		Definition{Name: "runner-only-goober", Version: 1, Spec: spec},
		WithGoobers(map[string]apiv1.GooberSpec{
			"coder": {Capabilities: []string{string(capability.ConfigRepoRead)}},
		}),
	)
	if err == nil || !strings.Contains(err.Error(), `goober "coder" grants runner-only capability "configrepo:read"`) {
		t.Fatalf("Compile error = %v, want runner-only goober capability rejection", err)
	}
}

func TestCompileCIPollRequiresProviderPRWrite(t *testing.T) {
	cases := []struct {
		name    string
		caps    []string
		wantErr string
	}{
		{
			name:    "missing required capability",
			wantErr: `task "poll" with inputs.kind="ci-poll" must declare capability "provider:pr:write"`,
		},
		{
			name: "required capability declared",
			caps: []string{string(capability.ProviderPRWrite)},
		},
		{
			name:    "provider-specific capability does not satisfy provider-neutral routing",
			caps:    []string{string(capability.GitHubPRWrite)},
			wantErr: `task "poll" with inputs.kind="ci-poll" must declare capability "provider:pr:write"`,
		},
		{
			name:    "provider-neutral and provider-specific capabilities conflict",
			caps:    []string{string(capability.ProviderPRWrite), string(capability.ADOPRWrite)},
			wantErr: `task "poll" declares mutually exclusive provider-neutral and provider-specific PR write capabilities`,
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
			_, err := compileAcknowledged(Definition{Name: "ci-poll", Version: 1, Spec: spec})
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

func TestCompileValidatesBuiltInProviderCapabilityManifest(t *testing.T) {
	queueTask := apiv1.Task{
		Name: "queue-watch",
		Type: apiv1.TaskDeterministic,
		Goal: "watch the merge queue",
		Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "merge-queue-poll"}},
		Capabilities: []string{
			string(capability.GitHubPRMerge),
			string(capability.GitHubBranchDelete),
		},
		PolicyActions: []string{"watch-merge-queue", "route-queue-outcome", "delete-branch"},
	}
	definition := func(name string, task apiv1.Task) Definition {
		return Definition{Name: name, Version: 1, Spec: apiv1.WorkflowSpec{
			Gaggle: "web",
			Start:  task.Name,
			Tasks:  []apiv1.Task{task},
		}}
	}

	readOnlyTask := apiv1.Task{
		Name: "read-backlog",
		Type: apiv1.TaskDeterministic,
		Goal: "confirm backlog access",
		Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--read-only"}},
	}
	_, err := compileAcknowledged(definition("no-capability-backlog-read", readOnlyTask))
	want := `task "read-backlog" invokes built-in subcommand "backlog-query" but does not declare capability "github:issues:read"`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Compile error = %v, want containing %q", err, want)
	}
	// Issue-read credentials are separately brokered, so a broader write grant
	// cannot satisfy this requirement.
	readOnlyTask.Capabilities = []string{string(capability.GitHubIssuesWrite)}
	if _, err := compileAcknowledged(definition("write-subsumes-backlog-read", readOnlyTask)); err == nil || !strings.Contains(err.Error(), "GOOBERS_CRED_GITHUB_ISSUES_READ") {
		t.Fatalf("read-only backlog-query with write capability should fail exact admission: %v", err)
	}
	readOnlyTask.Capabilities = []string{string(capability.GitHubIssuesRead)}
	if _, err := compileAcknowledged(definition("explicit-backlog-read", readOnlyTask)); err != nil {
		t.Fatalf("read-only backlog-query with explicit read capability should compile: %v", err)
	}

	_, err = compileAcknowledged(definition("missing-eviction-capability", queueTask))
	want = `task "queue-watch" invokes built-in subcommand "merge-queue-poll" but does not declare capability "github:issues:write"; the capability-scoped credential is not injected, so eviction remediation fails at runtime`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Compile error = %v, want containing %q", err, want)
	}

	queueTask.Capabilities = append(queueTask.Capabilities, string(capability.GitHubIssuesWrite))
	if _, err := compileAcknowledged(definition("complete-queue-capabilities", queueTask)); err != nil {
		t.Fatalf("merge-queue-poll with complete capabilities should compile: %v", err)
	}

	telemetryTask := apiv1.Task{
		Name:         "gather-signals",
		Type:         apiv1.TaskDeterministic,
		Goal:         "query telemetry",
		Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "telemetry-query"}},
		Capabilities: []string{string(capability.TelemetryRead)},
	}
	if _, err := compileAcknowledged(definition("ordinary-telemetry-query", telemetryTask)); err != nil {
		t.Fatalf("ordinary telemetry-query should not require Tutor's PR capability: %v", err)
	}
	telemetryCommands := []struct {
		name    string
		command []string
	}{
		{name: "double dash split", command: []string{"goobers", "telemetry-query", "--format", "tutor-live-verification"}},
		{name: "double dash equals", command: []string{"goobers", "telemetry-query", "--format=tutor-live-verification"}},
		{name: "single dash split", command: []string{"goobers", "telemetry-query", "-format", "tutor-live-verification"}},
		{name: "single dash equals", command: []string{"goobers", "telemetry-query", "-format=tutor-live-verification"}},
	}
	for _, tc := range telemetryCommands {
		t.Run(tc.name, func(t *testing.T) {
			telemetryTask.Run.Command = tc.command
			_, err := compileAcknowledged(definition("tutor-holdout-query", telemetryTask))
			if err == nil || !strings.Contains(err.Error(), `subcommand "telemetry-query" but does not declare capability "github:pr:write"`) {
				t.Fatalf("Tutor telemetry-query error = %v, want conditional PR capability diagnostic", err)
			}
		})
	}

	reconcileTask := apiv1.Task{
		Name: "reconcile",
		Type: apiv1.TaskDeterministic,
		Goal: "reconcile stale branches",
		Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "reconcile-branches"}},
	}
	reconcileCases := []struct {
		name          string
		definition    string
		inputs        map[string]string
		policyActions []string
	}{
		{name: "default dry-run", definition: "reconcile-dry-run"},
		{
			name:          "input-enabled deletion",
			definition:    "reconcile-input-delete",
			inputs:        map[string]string{"deleteBranches": "true"},
			policyActions: []string{"delete-branch"},
		},
	}
	for _, tc := range reconcileCases {
		t.Run(tc.name, func(t *testing.T) {
			task := reconcileTask
			task.Inputs = tc.inputs
			task.PolicyActions = tc.policyActions

			_, err := compileAcknowledged(definition(tc.definition, task))
			want := `task "reconcile" invokes built-in subcommand "reconcile-branches" but does not declare capability "github:branch:delete"; the capability-scoped credential is not injected, so stale branch reconciliation fails at runtime`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Compile error = %v, want containing %q", err, want)
			}

			task.Capabilities = []string{string(capability.GitHubBranchDelete)}
			if _, err := compileAcknowledged(definition(tc.definition+"-capable", task)); err != nil {
				t.Fatalf("reconcile-branches with branch capability should compile: %v", err)
			}
		})
	}
}

func TestCompileExternalTelemetryRequiresTelemetryRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
		ok   bool
	}{
		{name: "missing required capability"},
		{name: "required capability declared", caps: []string{string(capability.TelemetryRead)}, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web",
				Start:  "query",
				Tasks: []apiv1.Task{{
					Name:         "query",
					Type:         apiv1.TaskDeterministic,
					Goal:         "query operational telemetry",
					Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "external-telemetry"}},
					Inputs:       map[string]string{"kind": "external-telemetry", "connector": "metrics", "query": "health"},
					Capabilities: tc.caps,
				}},
			}
			_, err := compileAcknowledged(Definition{Name: "external-telemetry", Version: 1, Spec: spec})
			if tc.ok && err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), `must declare capability "telemetry:read"`)) {
				t.Fatalf("Compile error = %v", err)
			}
		})
	}
}

func TestCompilePolicyActionsRequireCapabilities(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "apply",
		Tasks: []apiv1.Task{{
			Name:          "apply",
			Type:          apiv1.TaskDeterministic,
			Goal:          "apply verdict",
			Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "apply-verdict"}},
			PolicyActions: []string{"publish-review", "route-provider-verdict", "close-pr"},
			Capabilities:  []string{string(capability.GitHubPRReview)},
		}},
	}

	_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `task "apply" policy action "close-pr" requires capability "provider:pr:write", but the task does not declare it`) {
		t.Fatalf("Compile error = %v, want missing policy-action capability", err)
	}

	spec.Tasks[0].Capabilities = append(spec.Tasks[0].Capabilities, string(capability.ProviderPRWrite))
	if _, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("policy actions with their capabilities should compile: %v", err)
	}
}

func TestCompilePolicyBearingCommandRequiresActionDeclarations(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "apply",
		Tasks: []apiv1.Task{{
			Name:         "apply",
			Type:         apiv1.TaskDeterministic,
			Goal:         "apply verdict",
			Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "apply-verdict"}},
			Capabilities: []string{string(capability.GitHubPRWrite), string(capability.GitHubPRReview)},
		}},
	}

	_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `task "apply" command "goobers apply-verdict" prescribes policy action "close-pr" but policyActions does not declare it`) {
		t.Fatalf("Compile error = %v, want missing policy-action declaration", err)
	}
}

func TestCompileSetMilestoneRequiresRoadmapPolicy(t *testing.T) {
	task := apiv1.Task{
		Name:         "milestone",
		Type:         apiv1.TaskDeterministic,
		Goal:         "assign roadmap milestone",
		Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "set-milestone"}},
		Capabilities: []string{string(capability.GitHubMilestonesWrite)},
	}
	spec := apiv1.WorkflowSpec{Gaggle: "web", Start: task.Name, Tasks: []apiv1.Task{task}}

	_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	const wantAction = `task "milestone" command "goobers set-milestone" prescribes policy action "assign-milestone" but policyActions does not declare it`
	if err == nil || !strings.Contains(err.Error(), wantAction) {
		t.Fatalf("Compile error = %v, want containing %q", err, wantAction)
	}

	spec.Tasks[0].PolicyActions = []string{"assign-milestone"}
	spec.Tasks[0].Capabilities = []string{string(capability.GitHubIssuesWrite)}
	_, err = compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	const wantCapability = `task "milestone" policy action "assign-milestone" requires capability "github:milestones:write", but the task does not declare it`
	if err == nil || !strings.Contains(err.Error(), wantCapability) {
		t.Fatalf("Compile error = %v, want containing %q", err, wantCapability)
	}
}

func TestCompileGatherSiblingContextRequiresPolicyActions(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "gather",
		Tasks: []apiv1.Task{{
			Name:         "gather",
			Type:         apiv1.TaskDeterministic,
			Goal:         "gather sibling context",
			Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "gather-sibling-context"}},
			Capabilities: []string{string(capability.GitHubPRWrite)},
		}},
	}

	_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	for _, action := range []string{"flag-scope-drift", "route-verdict"} {
		want := fmt.Sprintf(`command "goobers gather-sibling-context" prescribes policy action %q but policyActions does not declare it`, action)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Compile error = %v, want containing %q", err, want)
		}
	}

	spec.Tasks[0].PolicyActions = []string{"flag-scope-drift", "route-verdict"}
	spec.Tasks[0].Capabilities = nil
	_, err = compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	for _, action := range spec.Tasks[0].PolicyActions {
		want := fmt.Sprintf(`policy action %q requires capability "github:pr:write", but the task does not declare it`, action)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Compile error = %v, want containing %q", err, want)
		}
	}

	spec.Tasks[0].Capabilities = []string{string(capability.GitHubPRWrite)}
	if _, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("declared gather-sibling-context actions and capability should compile: %v", err)
	}
}

func TestCompileBacklogQueryBooleanPolicyActions(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantAction string
	}{
		{name: "long claim", args: []string{"--claim"}, wantAction: "claim-backlog-items"},
		{name: "short claim", args: []string{"-claim"}, wantAction: "claim-backlog-items"},
		{name: "long claim true", args: []string{"--claim=true"}, wantAction: "claim-backlog-items"},
		{name: "short claim true", args: []string{"-claim=true"}, wantAction: "claim-backlog-items"},
		{name: "claim before positional false", args: []string{"--claim", "false"}, wantAction: "claim-backlog-items"},
		{name: "long release", args: []string{"--release"}, wantAction: "release-backlog-claim"},
		{name: "short release", args: []string{"-release"}, wantAction: "release-backlog-claim"},
		{name: "long release true", args: []string{"--release=true"}, wantAction: "release-backlog-claim"},
		{name: "short release true", args: []string{"-release=true"}, wantAction: "release-backlog-claim"},
		{name: "long claim false", args: []string{"--claim=false"}},
		{name: "short claim false", args: []string{"-claim=false"}},
		{name: "long release false", args: []string{"--release=false"}},
		{name: "short release false", args: []string{"-release=false"}},
		{name: "flags terminated", args: []string{"--", "--claim"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command := append([]string{"goobers", "backlog-query"}, tc.args...)
			spec := apiv1.WorkflowSpec{
				Gaggle: "web",
				Start:  "query",
				Tasks: []apiv1.Task{{
					Name:         "query",
					Type:         apiv1.TaskDeterministic,
					Goal:         "query backlog",
					Run:          &apiv1.DeterministicRun{Command: command},
					Capabilities: []string{string(capability.GitHubIssuesWrite)},
				}},
			}

			_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
			if tc.wantAction == "" {
				if err != nil {
					t.Fatalf("non-mutating boolean form should compile: %v", err)
				}
				return
			}
			want := fmt.Sprintf(`command "goobers backlog-query" prescribes policy action %q`, tc.wantAction)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Compile error = %v, want containing %q", err, want)
			}
		})
	}
}

func TestBacklogQueryReconciliationPolicyActions(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		inputs     map[string]string
		inputsFrom map[string]string
		want       string
	}{
		{name: "direct reconciliation", args: []string{"--reconcile"}, want: "claim-backlog-items,close-issue"},
		{name: "renamed curation default cardinality", args: []string{"--claim"}, inputs: map[string]string{"curation": "true"}, want: "claim-backlog-items,close-issue"},
		{name: "renamed curation single item", args: []string{"--claim"}, inputs: map[string]string{"curation": "true", "maxItems": "1"}, want: "claim-backlog-items,close-issue"},
		{name: "renamed curation batch defaults to reconciliation", args: []string{"--claim"}, inputs: map[string]string{"curation": "true", "maxItems": "20"}, want: "claim-backlog-items,close-issue"},
		{name: "curation batch conservatively declares reconciliation", args: []string{"--claim"}, inputs: map[string]string{"curation": "true", "maxItems": "20", "reconcileMetadata": "false"}, want: "claim-backlog-items,close-issue"},
		{name: "dynamic curation mode conservatively declares reconciliation", args: []string{"--claim"}, inputsFrom: map[string]string{"curation": "curationMode"}, want: "claim-backlog-items,close-issue"},
		{name: "single-item implementation claim", args: []string{"--claim"}, inputs: map[string]string{"maxItems": "1"}, want: "claim-backlog-items"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := apiv1.Task{
				Run:        &apiv1.DeterministicRun{Command: append([]string{"goobers", "backlog-query"}, tc.args...)},
				Inputs:     tc.inputs,
				InputsFrom: tc.inputsFrom,
			}
			got := strings.Join(prescribedCommandPolicyActions(task), ",")
			if got != tc.want {
				t.Fatalf("policy actions = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompileBacklogHealthFeedbackRequiresUpdateAction(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "feedback",
		Tasks: []apiv1.Task{{
			Name:         "feedback",
			Type:         apiv1.TaskDeterministic,
			Goal:         "re-curate chronically failing items",
			Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-health", "--feedback"}},
			Capabilities: []string{string(capability.GitHubIssuesWrite)},
		}},
	}

	_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	const want = `command "goobers backlog-health" prescribes policy action "update-issue"`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Compile error = %v, want containing %q", err, want)
	}

	spec.Tasks[0].PolicyActions = []string{"update-issue"}
	if _, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("declared feedback action and capability should compile: %v", err)
	}
}

func TestRespondToFindingsCheckDoesNotPrescribeMutation(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		wantMutation bool
	}{
		{name: "long flag", args: []string{"--check"}},
		{name: "short flag", args: []string{"-check"}},
		{name: "explicit true", args: []string{"--check=true"}},
		{name: "before path", args: []string{"--check", "path"}},
		{name: "explicit false", args: []string{"--check=false"}, wantMutation: true},
		{name: "overridden false", args: []string{"--check", "--check=false"}, wantMutation: true},
		{name: "after path", args: []string{"path", "--check"}, wantMutation: true},
		{name: "after terminator", args: []string{"--", "--check"}, wantMutation: true},
		{name: "missing flag", wantMutation: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := apiv1.Task{
				Run: &apiv1.DeterministicRun{
					Command: append([]string{"goobers", "respond-to-findings"}, tc.args...),
				},
			}
			got := prescribedCommandPolicyActions(task)
			if tc.wantMutation {
				if len(got) != 1 || got[0] != "respond-to-findings" {
					t.Fatalf("policy actions = %v, want [respond-to-findings]", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("policy actions = %v, want none", got)
			}
		})
	}
}

func TestCompileReconcileBranchesDeletePolicyAction(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		inputs     map[string]string
		wantAction bool
	}{
		{name: "long flag", args: []string{"--delete"}, wantAction: true},
		{name: "short flag", args: []string{"-delete"}, wantAction: true},
		{name: "explicit true", args: []string{"--delete=true"}, wantAction: true},
		{name: "after unrelated flag", args: []string{"--max", "5", "--delete"}, wantAction: true},
		{name: "before unrelated flag", args: []string{"--delete", "--max", "5"}, wantAction: true},
		{name: "explicit false", args: []string{"--delete=false"}},
		{name: "flags terminated", args: []string{"--", "--delete"}},
		{name: "input true", inputs: map[string]string{"deleteBranches": "true"}, wantAction: true},
		{name: "input false", inputs: map[string]string{"deleteBranches": "false"}},
		{name: "flag overrides input", args: []string{"--delete=false"}, inputs: map[string]string{"deleteBranches": "true"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web",
				Start:  "reconcile",
				Tasks: []apiv1.Task{{
					Name:         "reconcile",
					Type:         apiv1.TaskDeterministic,
					Goal:         "reconcile stale branches",
					Run:          &apiv1.DeterministicRun{Command: append([]string{"goobers", "reconcile-branches"}, tc.args...)},
					Inputs:       tc.inputs,
					Capabilities: []string{string(capability.GitHubBranchDelete)},
				}},
			}

			_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
			if !tc.wantAction {
				if err != nil {
					t.Fatalf("non-deleting reconciliation should compile: %v", err)
				}
				return
			}
			const want = `command "goobers reconcile-branches" prescribes policy action "delete-branch"`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Compile error = %v, want containing %q", err, want)
			}
		})
	}

	task := apiv1.Task{
		Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "reconcile-branches"}},
		InputsFrom: map[string]string{"deleteBranches": "enabled"},
	}
	if got := prescribedCommandPolicyActions(task); len(got) != 1 || got[0] != "delete-branch" {
		t.Fatalf("dynamic deleteBranches actions = %v, want [delete-branch]", got)
	}
	task.Run.Command = append(task.Run.Command, "--delete=false")
	if got := prescribedCommandPolicyActions(task); len(got) != 0 {
		t.Fatalf("explicitly disabled dynamic deleteBranches actions = %v, want none", got)
	}

	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "reconcile",
		Tasks: []apiv1.Task{{
			Name:          "reconcile",
			Type:          apiv1.TaskDeterministic,
			Goal:          "delete eligible stale branches",
			Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "reconcile-branches", "--delete"}},
			PolicyActions: []string{"delete-branch"},
		}},
	}
	_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	const wantCapability = `policy action "delete-branch" requires capability "github:branch:delete"`
	if err == nil || !strings.Contains(err.Error(), wantCapability) {
		t.Fatalf("Compile error = %v, want containing %q", err, wantCapability)
	}
	spec.Tasks[0].Capabilities = []string{string(capability.GitHubBranchDelete)}
	if _, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("declared branch deletion action and capability should compile: %v", err)
	}
}

func TestCompileMutationCapabilityWithoutPrescribedAction(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{{
			Name:         "implement",
			Type:         apiv1.TaskAgentic,
			Goober:       "implementer",
			Goal:         "run a fixture agent",
			Capabilities: []string{string(capability.RepoPush), string(capability.AgentModel)},
		}},
	}
	goobers := map[string]apiv1.GooberSpec{
		"implementer": {Capabilities: []string{string(capability.RepoPush), string(capability.AgentModel)}},
	}

	if _, err := compileAcknowledged(
		Definition{Name: "policy", Version: 1, Spec: spec},
		WithGoobers(goobers),
	); err != nil {
		t.Fatalf("mutation capability without a prescribed action should compile: %v", err)
	}
}

func TestCompileAgenticPersonaActionsAreLoadBearing(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{{
			Name:         "implement",
			Type:         apiv1.TaskAgentic,
			Goober:       "implementer",
			Goal:         "remediate the pull request",
			Capabilities: []string{string(capability.RepoPush), string(capability.AgentModel)},
		}},
	}
	goobers := map[string]apiv1.GooberSpec{
		"implementer": {
			Capabilities:  []string{string(capability.RepoPush), string(capability.AgentModel)},
			PolicyActions: []string{"modify-repository"},
		},
	}

	_, err := compileAcknowledged(
		Definition{Name: "policy", Version: 1, Spec: spec},
		WithGoobers(goobers),
	)
	const want = `task "implement" invokes goober "implementer" whose persona prescribes policy action "modify-repository", but policyActions does not declare it`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Compile error = %v, want containing %q", err, want)
	}

	spec.Tasks[0].PolicyActions = []string{"modify-repository"}
	if _, err := compileAcknowledged(
		Definition{Name: "policy", Version: 1, Spec: spec},
		WithGoobers(goobers),
	); err != nil {
		t.Fatalf("declared persona action should compile: %v", err)
	}
}

func TestCompileConditionalPersonaActionRequiresTaskOptIn(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{
		"nominator": {
			Capabilities:             []string{string(capability.GitHubIssuesWrite), string(capability.GitHubIssuesApprove), string(capability.AgentModel)},
			PolicyActions:            []string{"create-issue"},
			ConditionalPolicyActions: []string{"approve-issue"},
		},
	}

	cases := []struct {
		name          string
		capabilities  []string
		policyActions []string
		wantErr       string
	}{
		{
			name:          "disabled",
			capabilities:  []string{string(capability.GitHubIssuesWrite)},
			policyActions: []string{"create-issue"},
		},
		{
			name:          "capability without action",
			capabilities:  []string{string(capability.GitHubIssuesWrite), string(capability.GitHubIssuesApprove)},
			policyActions: []string{"create-issue"},
			wantErr:       `task "nominate" grants capability "github:issues:approve" for goober "nominator" conditional policy action "approve-issue", but policyActions does not declare it`,
		},
		{
			name:          "action without capability",
			capabilities:  []string{string(capability.GitHubIssuesWrite)},
			policyActions: []string{"create-issue", "approve-issue"},
			wantErr:       `task "nominate" policy action "approve-issue" requires capability "github:issues:approve", but the task does not declare it`,
		},
		{
			name:          "action and capability",
			capabilities:  []string{string(capability.GitHubIssuesWrite), string(capability.GitHubIssuesApprove)},
			policyActions: []string{"create-issue", "approve-issue"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web",
				Start:  "nominate",
				Tasks: []apiv1.Task{{
					Name:          "nominate",
					Type:          apiv1.TaskAgentic,
					Goober:        "nominator",
					Goal:          "file evidence-backed issues",
					Capabilities:  append(tc.capabilities, string(capability.AgentModel)),
					PolicyActions: tc.policyActions,
				}},
			}
			_, err := compileAcknowledged(
				Definition{Name: "policy", Version: 1, Spec: spec},
				WithGoobers(goobers),
			)
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

func TestCompilePersonaActionRequiresGooberCapability(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "curate",
		Tasks: []apiv1.Task{{
			Name:          "curate",
			Type:          apiv1.TaskAgentic,
			Goober:        "curator",
			Goal:          "curate issues",
			Capabilities:  []string{string(capability.GitHubIssuesWrite)},
			PolicyActions: []string{"close-issue"},
		}},
	}
	goobers := map[string]apiv1.GooberSpec{
		"curator": {
			Capabilities:  []string{string(capability.AgentModel)},
			PolicyActions: []string{"close-issue"},
		},
	}

	_, err := compileAcknowledged(
		Definition{Name: "policy", Version: 1, Spec: spec},
		WithGoobers(goobers),
	)
	const want = `goober "curator" policy action "close-issue" requires capability "github:issues:write", but the goober does not grant it`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Compile error = %v, want containing %q", err, want)
	}
}

func TestCompileValidatesAgenticGatePersonaActions(t *testing.T) {
	spec := gatedSpec()
	goobers := map[string]apiv1.GooberSpec{
		"reviewer": {
			Capabilities:  []string{string(capability.AgentModel)},
			PolicyActions: []string{"close-issue", "retarget-pr"},
		},
	}

	_, err := compileAcknowledged(
		Definition{Name: "policy", Version: 1, Spec: spec},
		WithGoobers(goobers),
	)
	if err == nil {
		t.Fatal("Compile should reject invalid policy actions on a gate-only goober")
	}
	for _, want := range []string{
		`goober "reviewer" policy action "close-issue" requires capability "github:issues:write", but the goober does not grant it`,
		`goober "reviewer" declares unknown policy action "retarget-pr"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Compile error = %v, want containing %q", err, want)
		}
	}
}

func TestCompileRejectsPolicyBearingAgenticGatePersonas(t *testing.T) {
	cases := []struct {
		name        string
		policy      []string
		conditional []string
	}{
		{name: "unconditional", policy: []string{"comment-on-issue"}},
		{name: "conditional", conditional: []string{"comment-on-issue"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := gatedSpec()
			goobers := map[string]apiv1.GooberSpec{
				"reviewer": {
					Capabilities:             []string{string(capability.AgentModel), string(capability.GitHubIssuesWrite)},
					PolicyActions:            tc.policy,
					ConditionalPolicyActions: tc.conditional,
				},
			}

			_, err := compileAcknowledged(
				Definition{Name: "policy", Version: 1, Spec: spec},
				WithGoobers(goobers),
			)
			const want = `agentic gate "review" invokes goober "reviewer" whose persona prescribes policy action "comment-on-issue", but agentic gates cannot opt into policy actions`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Compile error = %v, want containing %q", err, want)
			}
		})
	}
}

func TestCompileRejectsUnknownAndDuplicatePolicyActions(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "act",
		Tasks: []apiv1.Task{{
			Name:          "act",
			Type:          apiv1.TaskAgentic,
			Goal:          "act",
			PolicyActions: []string{"rework-pr", "retarget-pr", "rework-pr"},
			Capabilities:  []string{string(capability.RepoPush)},
		}},
	}

	_, err := compileAcknowledged(Definition{Name: "policy", Version: 1, Spec: spec})
	if err == nil {
		t.Fatal("Compile should reject invalid policy actions")
	}
	for _, want := range []string{
		`task "act" declares unknown policy action "retarget-pr"`,
		`task "act" declares duplicate policy action "rework-pr"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Compile error = %v, want containing %q", err, want)
		}
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
	_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `check "ci-status" params.equals "success" is not one of`) {
		t.Fatalf("expected a gate-vocabulary-mismatch error, got %v", err)
	}

	// The correct vocabulary for ci-status compiles clean.
	spec.Gates[0].Automated.Params["equals"] = "passing"
	if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("correct ci-status vocabulary should compile, got %v", err)
	}

	// status-equals uses the opposite (apiv1.ResultStatus) vocabulary —
	// "passing" is invalid there too.
	spec.Gates[0].Automated.Check = "status-equals"
	spec.Gates[0].Automated.Params["equals"] = "passing"
	_, err = compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
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
			if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
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
			_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
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
	_, err := compileAcknowledged(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}))

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
	_, err := compileAcknowledged(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}))

	if err == nil || !strings.Contains(err.Error(), `task "implement" declares unknown capability "github:pulls:write"`) {
		t.Fatalf("expected unknown-capability-declared error, got %v", err)
	}
}

func TestCompileAdmissionUnknownHarness(t *testing.T) {
	spec := linearSpec()
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.Harness("nonesuch")},
	}
	_, err := compileAcknowledged(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}))

	if err == nil || !strings.Contains(err.Error(), `unknown harness "nonesuch"`) {
		t.Fatalf("expected unknown-harness error, got %v", err)
	}
}

func TestCompileAdmissionUsesRegisteredHarnessNames(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{
		"coder": {Role: "coder", Harness: apiv1.Harness("alternate")},
	}
	def := Definition{Name: "x", Version: 1, Spec: linearSpec()}

	if _, err := compileAcknowledged(def, WithGoobers(goobers), WithKnownHarnesses([]string{"alternate"})); err != nil {
		t.Fatalf("registered harness should compile, got %v", err)
	}
	if _, err := compileAcknowledged(def, WithGoobers(goobers), WithKnownHarnesses(nil)); err == nil ||
		!strings.Contains(err.Error(), `unknown harness "alternate"`) {
		t.Fatalf("unregistered harness should fail closed, got %v", err)
	}
}

func TestCompileAdmissionRequiresModelCapabilityForTokenBackedHarness(t *testing.T) {
	spec := linearSpec()
	spec.Tasks[0].Capabilities = []string{string(capability.AgentModel)}

	tests := []struct {
		name         string
		harness      apiv1.Harness
		gooberCaps   []string
		taskCaps     []string
		wantErr      string
		wantAccepted bool
	}{
		{
			name:     "goober grant missing",
			harness:  apiv1.HarnessCopilot,
			taskCaps: []string{string(capability.AgentModel)},
			wantErr:  `task "implement" uses goober "coder" (harness: copilot) but the goober does not grant capability "agent:model"; the harness will receive no model credential`,
		},
		{
			name:       "task declaration missing",
			harness:    apiv1.HarnessCopilot,
			gooberCaps: []string{string(capability.AgentModel)},
			wantErr:    `task "implement" uses goober "coder" (harness: copilot) but does not declare capability "agent:model"; the harness will receive no model credential`,
		},
		{
			name:         "both declared",
			harness:      apiv1.HarnessCopilot,
			gooberCaps:   []string{string(capability.AgentModel)},
			taskCaps:     []string{string(capability.AgentModel)},
			wantAccepted: true,
		},
		{
			name:         "custom harness does not require platform model credential",
			harness:      apiv1.Harness("alternate"),
			wantAccepted: true,
		},
		{
			name:     "claude code goober grant missing",
			harness:  apiv1.HarnessClaudeCode,
			taskCaps: []string{string(capability.AgentModel)},
			wantErr:  `task "implement" uses goober "coder" (harness: claude-code) but the goober does not grant capability "agent:model"; the harness will receive no model credential`,
		},
		{
			name:       "claude code task declaration missing",
			harness:    apiv1.HarnessClaudeCode,
			gooberCaps: []string{string(capability.AgentModel)},
			wantErr:    `task "implement" uses goober "coder" (harness: claude-code) but does not declare capability "agent:model"; the harness will receive no model credential`,
		},
		{
			name:         "claude code both declared",
			harness:      apiv1.HarnessClaudeCode,
			gooberCaps:   []string{string(capability.AgentModel)},
			taskCaps:     []string{string(capability.AgentModel)},
			wantAccepted: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec.Tasks[0].Capabilities = tc.taskCaps
			_, err := compileAcknowledged(
				Definition{Name: "x", Version: 1, Spec: spec},
				WithGoobers(map[string]apiv1.GooberSpec{
					"coder": {Role: "coder", Harness: tc.harness, Capabilities: tc.gooberCaps},
				}),
				WithKnownHarnesses([]string{string(tc.harness)}),
			)
			if tc.wantAccepted {
				if err != nil {
					t.Fatalf("Compile() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Compile() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCompileAdmissionRequiresReviewerModelCapability(t *testing.T) {
	spec := gatedSpec()
	spec.Tasks[0].Capabilities = []string{string(capability.AgentModel)}
	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			Role:         "coder",
			Harness:      apiv1.HarnessCopilot,
			Capabilities: []string{string(capability.AgentModel)},
		},
		"reviewer": {Role: "reviewer", Harness: apiv1.HarnessCopilot},
	}

	_, err := compileAcknowledged(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(goobers),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}),
	)
	want := `gate "review" reviewer uses goober "reviewer" (harness: copilot) but the goober does not grant capability "agent:model"; the harness will receive no model credential`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Compile() error = %v, want containing %q", err, want)
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
	_, err := compileAcknowledged(
		Definition{Name: "x", Version: 1, Spec: spec},
		WithGoobers(map[string]apiv1.GooberSpec{}),
		WithKnownHarnesses([]string{string(apiv1.HarnessCopilot)}))

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
		_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
		if err == nil || !strings.Contains(err.Error(), `gate "review": branch "reject" is not a producible outcome`) {
			t.Fatalf("expected unproducible-branch error, got %v", err)
		}
	})

	t.Run("missing producible outcome", func(t *testing.T) {
		spec := agenticGate(map[string]string{"pass": TerminalComplete, "fail": TargetAbort}) // no needs-changes
		_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
		if err == nil || !strings.Contains(err.Error(), `gate "review": producible outcome "needs-changes" has no branch`) {
			t.Fatalf("expected missing-outcome error, got %v", err)
		}
	})

	t.Run("full coverage compiles", func(t *testing.T) {
		spec := agenticGate(map[string]string{"pass": TerminalComplete, "fail": TargetAbort, "needs-changes": "implement"})
		if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
			t.Fatalf("full outcome coverage should compile, got %v", err)
		}
	})

	t.Run("escalation control branch compiles", func(t *testing.T) {
		spec := agenticGate(map[string]string{
			"pass": TerminalComplete, "fail": TargetAbort, "needs-changes": "implement",
			BranchEscalate: TargetAbort,
		})
		if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
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
		_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
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
		_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
		if err == nil || !strings.Contains(err.Error(), `gate "gate-only": producible outcome "enqueued" has no branch`) {
			t.Fatalf("expected missing-enqueued-branch error, got %v", err)
		}
	})

	t.Run("all missing outcomes share one diagnostic", func(t *testing.T) {
		spec := apiv1.WorkflowSpec{
			Gaggle: "web",
			Start:  "gate-only",
			Gates: []apiv1.Gate{{
				Name: "gate-only", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "land-outcome"},
				Branches: map[string]string{"merged": TerminalComplete},
			}},
		}
		problems := CheckGateOutcomes(Definition{Name: "x", Version: 1, Spec: spec})
		if len(problems) != 1 {
			t.Fatalf("CheckGateOutcomes returned %d problems, want one diagnostic: %v", len(problems), problems)
		}
		want := `gate "gate-only": producible outcomes "enqueued", "fail" have no branches (would fail closed at evaluation time)`
		if problems[0] != want {
			t.Fatalf("CheckGateOutcomes problem = %q, want %q", problems[0], want)
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
		if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
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
		_, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec})
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

	_, err := compileAcknowledged(def, WithKnownChecks([]string{"status-equals", "ci-status"}))
	if err == nil || !strings.Contains(err.Error(), `gate "gate-only": unknown automated check "ci-green"`) {
		t.Fatalf("expected unknown-check error, got %v", err)
	}

	// Without WithKnownChecks (the runner path default), check names are not
	// validated — internal/gate itself still fails closed at evaluation time
	// regardless, per the doc comment on WithKnownChecks.
	if _, err := compileAcknowledged(def); err != nil {
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
	if _, err := compileAcknowledged(Definition{Name: "x", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("runner path should not run admission, got %v", err)
	}
}
