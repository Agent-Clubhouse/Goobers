package v30

// gate_runson_test.go covers the gates[].runsOn half of decision 001
// (Goobernetes-E2E-Core, #3798): agentic gates are placeable through the
// identical runsOn block tasks carry; automated and human gates never are
// (WF023); a placed gate must declare cpu and memory (WF023, ruling 5); the
// CAP004/CAP005/structural checks read gate blocks exactly as task blocks;
// and StagePlacements emits a requirement row per placed agentic gate after
// the task rows, deriving harness:<reviewer harness> from the gate's goober.

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runnersolve"
)

// placedReviewRunsOn is a ruling-5-complete gate block: cpu and memory
// present, plus a restriction so a solver row is not self-satisfiable.
func placedReviewRunsOn() *apiv1.RunsOn {
	return &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi", Restrictions: []string{"network:allowlist"}}
}

// gatedSpecWithReviewRunsOn is gatedSpec with runsOn declared on its agentic
// review gate, optionally mutated further.
func gatedSpecWithReviewRunsOn(runsOn *apiv1.RunsOn, mutate func(*apiv1.Gate)) apiv1.WorkflowSpec {
	spec := gatedSpec()
	spec.Gates[0].RunsOn = runsOn
	if mutate != nil {
		mutate(&spec.Gates[0])
	}
	return spec
}

func TestCompileAcceptsPlacedAgenticGate(t *testing.T) {
	def := Definition{Name: "placed-gate", Version: 1, Spec: gatedSpecWithReviewRunsOn(placedReviewRunsOn(), nil)}
	if _, err := compileAcknowledged(def); err != nil {
		t.Fatalf("an agentic gate with runsOn{cpu,memory} must compile (decision 001 ruling 1): %v", err)
	}
}

// Ruling 2: runsOn on a non-agentic gate is a compile error with a pointer,
// never a silently ignored block. The same document without runsOn compiles,
// so the refusal is attributable to WF023 alone.
func TestCompileRefusesRunsOnOnNonAgenticGate(t *testing.T) {
	automated := func(g *apiv1.Gate) {
		g.Evaluator = apiv1.EvaluatorAutomated
		g.Agentic = nil
		g.Automated = &apiv1.AutomatedGate{Check: "status-equals"}
		g.Branches = map[string]string{"pass": TerminalComplete, "fail": TargetAbort}
	}
	baseline := Definition{Name: "automated-gate", Version: 1, Spec: gatedSpecWithReviewRunsOn(nil, automated)}
	if _, err := compileAcknowledged(baseline); err != nil {
		t.Fatalf("baseline automated gate must compile: %v", err)
	}
	def := Definition{Name: "automated-gate", Version: 1, Spec: gatedSpecWithReviewRunsOn(placedReviewRunsOn(), automated)}
	_, err := compileAcknowledged(def)
	if err == nil || !strings.Contains(err.Error(), `gate "review" declares runsOn but its evaluator is "automated"`) {
		t.Fatalf("Compile error = %v, want the WF023 non-agentic refusal", err)
	}
	if !strings.Contains(err.Error(), "decision 001") {
		t.Fatalf("Compile error = %v, want the decision pointer", err)
	}
}

// Ruling 5: a placed gate must carry quantities. Every missing-quantity shape
// is named in the message so the fix is a one-line edit.
func TestCompileRefusesPlacedGateWithoutQuantities(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runsOn *apiv1.RunsOn
		want   string
	}{
		{"missing cpu", &apiv1.RunsOn{Memory: "2Gi"}, `gate "review" declares runsOn without cpu:`},
		{"missing memory", &apiv1.RunsOn{CPU: "1000m"}, `gate "review" declares runsOn without memory:`},
		{"missing both", &apiv1.RunsOn{OS: "linux"}, `gate "review" declares runsOn without cpu and memory:`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := Definition{Name: "under-provisioned", Version: 1, Spec: gatedSpecWithReviewRunsOn(tc.runsOn, nil)}
			_, err := compileAcknowledged(def)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile error = %v, want %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "ruling 5") {
				t.Fatalf("Compile error = %v, want the ruling-5 pointer", err)
			}
		})
	}
}

// CAP004, CAP005 and the structural runsOn checks read a gate block by the
// same rule as a task block, naming the gate in the diagnostic.
func TestGateRunsOnVocabularyChecks(t *testing.T) {
	t.Run("CAP004 os token", func(t *testing.T) {
		def := Definition{Spec: gatedSpecWithReviewRunsOn(&apiv1.RunsOn{CPU: "1000m", Memory: "2Gi", Capabilities: []string{"os=linux"}}, nil)}
		got := CheckRunsOnOSTokens(def, nil)
		if len(got) != 1 || !strings.Contains(got[0], `gate "review" runsOn.capabilities contains "os=linux"`) {
			t.Fatalf("CheckRunsOnOSTokens = %v, want the gate-attributed CAP004", got)
		}
	})
	t.Run("CAP005 unknown restriction", func(t *testing.T) {
		def := Definition{Spec: gatedSpecWithReviewRunsOn(&apiv1.RunsOn{CPU: "1000m", Memory: "2Gi", Restrictions: []string{"network:allow-list"}}, nil)}
		got := CheckRunsOnRestrictions(def, nil)
		if len(got) != 1 || !strings.HasPrefix(got[0], `gate "review" runsOn.restrictions:`) || !strings.Contains(got[0], `did you mean "network:allowlist"?`) {
			t.Fatalf("CheckRunsOnRestrictions = %v, want the gate-attributed CAP005 with a suggestion", got)
		}
	})
	t.Run("malformed quantity", func(t *testing.T) {
		def := Definition{Spec: gatedSpecWithReviewRunsOn(&apiv1.RunsOn{CPU: "lots", Memory: "2Gi"}, nil)}
		got := CheckRunsOnPlacement(def, nil)
		if len(got) != 1 || !strings.Contains(got[0], `gate "review" runsOn.cpu "lots" must be a Kubernetes quantity string`) {
			t.Fatalf("CheckRunsOnPlacement = %v, want the gate-attributed quantity error", got)
		}
	})
	t.Run("gaggle OS conflict", func(t *testing.T) {
		def := Definition{Spec: gatedSpecWithReviewRunsOn(&apiv1.RunsOn{OS: "windows", CPU: "1000m", Memory: "2Gi"}, nil)}
		got := CheckRunsOnPlacement(def, &apiv1.GaggleRunsOn{OS: "linux"})
		if len(got) != 1 || !strings.Contains(got[0], `gate "review" runsOn.os "windows" conflicts with the gaggle-level runsOn.os "linux"`) {
			t.Fatalf("CheckRunsOnPlacement = %v, want the gate-attributed OS conflict", got)
		}
	})
	t.Run("task diagnostics unchanged", func(t *testing.T) {
		// The task spelling is byte-identical to before gates joined the
		// loop: "task" is the kind label, not a new prefix.
		task := implementTask("implement", &apiv1.RunsOn{CPU: "lots"})
		got := CheckRunsOnPlacement(Definition{Spec: singleTaskSpec(task)}, nil)
		if len(got) != 1 || !strings.HasPrefix(got[0], `task "implement" runsOn.cpu "lots" must be a Kubernetes quantity string`) {
			t.Fatalf("CheckRunsOnPlacement = %v, want the unchanged task diagnostic", got)
		}
	})
}

// An agentic gate derives harness:<reviewer harness> from ITS goober through
// the same rule an agentic task uses (D7); other evaluators derive nothing.
func TestDerivedGateCapabilities(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{
		"reviewer": {Gaggle: "web", Harness: apiv1.HarnessClaudeCode},
		"coder":    {Gaggle: "web", Harness: apiv1.HarnessCopilot},
	}
	review := gatedSpec().Gates[0]
	if got := DerivedGateCapabilities(review, goobers); len(got) != 1 || got[0] != "harness:claude-code" {
		t.Fatalf("agentic gate derived = %v, want [harness:claude-code] (the REVIEWER's harness, not the task goober's)", got)
	}
	if got := DerivedGateCapabilities(review, nil); len(got) != 1 || got[0] != "harness:copilot" {
		t.Fatalf("nil goober map derived = %v, want the schema-default [harness:copilot]", got)
	}
	automated := apiv1.Gate{Name: "ci", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "ci-status"}}
	if got := DerivedGateCapabilities(automated, goobers); len(got) != 0 {
		t.Fatalf("automated gate derived = %v, want none (control plane by definition)", got)
	}
	human := apiv1.Gate{Name: "approve", Evaluator: apiv1.EvaluatorHuman, Human: &apiv1.HumanGate{}}
	if got := DerivedGateCapabilities(human, goobers); len(got) != 0 {
		t.Fatalf("human gate derived = %v, want none", got)
	}
}

// StagePlacements emits one row per task in task order, then one row per
// AGENTIC gate that declares runsOn, in gate order — with the reviewer's
// harness derived and the gaggle floor merged exactly as for a task. An
// agentic gate without runsOn, an automated gate (even one carrying a block
// WF023 will refuse), and a human gate emit nothing.
func TestStagePlacementsEmitsPlacedAgenticGatesAfterTasks(t *testing.T) {
	def := Definition{
		Name: "wf", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "web", Start: "implement",
			Tasks: []apiv1.Task{
				{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "review"},
				{Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "ci", Run: &apiv1.DeterministicRun{Command: []string{"make", "ci"}}},
			},
			Gates: []apiv1.Gate{
				{
					Name: "review", Evaluator: apiv1.EvaluatorAgentic,
					Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
					RunsOn:   &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi", Disk: "10Gi", Capabilities: []string{"git"}, Restrictions: []string{"network:allowlist"}},
					Branches: map[string]string{"pass": "local-ci", "fail": TargetAbort},
				},
				{
					Name: "ci", Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: "ci-status"},
					RunsOn:    &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi"},
					Branches:  map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
				},
				{
					Name: "unplaced", Evaluator: apiv1.EvaluatorAgentic,
					Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
					Branches: map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
				},
				{
					Name: "approve", Evaluator: apiv1.EvaluatorHuman,
					Human:    &apiv1.HumanGate{},
					Branches: map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
				},
			},
		},
	}
	floor := &apiv1.GaggleRunsOn{Capabilities: []string{"go@1.26"}, Restrictions: []string{"tmp:ephemeral"}}
	goobers := map[string]apiv1.GooberSpec{
		"coder":    {Harness: apiv1.HarnessClaudeCode},
		"reviewer": {Harness: apiv1.HarnessCopilot},
	}
	got := StagePlacements(def, floor, goobers)
	want := []runnersolve.StageRequirement{
		{Stage: "implement", Capabilities: []string{"go@1.26", "harness:claude-code"}, Restrictions: []string{"tmp:ephemeral"}},
		{Stage: "local-ci", Capabilities: []string{"go@1.26", "run:shell"}, Restrictions: []string{"tmp:ephemeral"}},
		{
			Stage: "review", CPU: "1000m", Memory: "2Gi", Disk: "10Gi",
			Capabilities: []string{"git", "go@1.26", "harness:copilot"},
			Restrictions: []string{"network:allowlist", "tmp:ephemeral"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StagePlacements =\n  %#v\nwant\n  %#v", got, want)
	}
}

// The registry records a gate's runsOn use so `goobers features --used` and
// the feature-support checks see the field.
func TestFeaturesForWorkflowCollectsGateRunsOn(t *testing.T) {
	def := Definition{Name: "placed-gate", Version: 1, Spec: gatedSpecWithReviewRunsOn(
		&apiv1.RunsOn{OS: "linux", CPU: "1000m", Memory: "2Gi", Disk: "10Gi", Capabilities: []string{"git"}, Restrictions: []string{"network:allowlist"}}, nil)}
	features, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	seen := map[FeatureID]bool{}
	for _, feature := range features {
		seen[feature.ID] = true
	}
	for _, id := range []FeatureID{
		featureGateRunsOn, featureGateRunsOnOS, featureGateRunsOnCPU, featureGateRunsOnMemory,
		featureGateRunsOnDisk, featureGateRunsOnCapabilities, featureGateRunsOnRestrictions,
	} {
		if !seen[id] {
			t.Errorf("feature %q not reported for a gate declaring the field", id)
		}
		feature, ok := currentFeatureRegistry.Lookup(id)
		if !ok {
			t.Fatalf("feature %q is not registered", id)
		}
		if feature.Level != SupportGA {
			t.Errorf("feature %q level = %q, want GA-within-3.0 (ruling 1: never a VER002 preview feature)", id, feature.Level)
		}
	}
	unplaced, err := FeaturesForWorkflow(Definition{Name: "unplaced", Version: 1, Spec: gatedSpec()})
	if err != nil {
		t.Fatalf("FeaturesForWorkflow (unplaced): %v", err)
	}
	for _, feature := range unplaced {
		if strings.HasPrefix(string(feature.ID), "gate.runsOn") {
			t.Fatalf("feature %q reported for a gate declaring no runsOn", feature.ID)
		}
	}
}
