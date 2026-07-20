package workflow

import (
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestFeatureRegistryLookup(t *testing.T) {
	for _, id := range []FeatureID{
		"trigger.signal",
		"gate.evaluator.human",
		"task.retry.backoff",
		"goober.spec.model",
	} {
		feature, ok := LookupFeature(id)
		if !ok {
			t.Fatalf("LookupFeature(%q) was not found", id)
		}
		if feature.ID != id {
			t.Errorf("LookupFeature(%q).ID = %q", id, feature.ID)
		}
	}
	if _, ok := LookupFeature("unknown.feature"); ok {
		t.Fatal("LookupFeature(unknown.feature) unexpectedly succeeded")
	}
}

func TestCurrentFeaturesArePreviewAtInitialVersion(t *testing.T) {
	features := AllFeatures()
	if len(features) == 0 {
		t.Fatal("feature registry is empty")
	}
	for _, feature := range features {
		if feature.Level != SupportPreview {
			t.Errorf("feature %q level = %q, want %q", feature.ID, feature.Level, SupportPreview)
		}
		if feature.SinceVersion != initialFeatureSinceVersion {
			t.Errorf("feature %q since-version = %q, want initial app version %q", feature.ID, feature.SinceVersion, initialFeatureSinceVersion)
		}
	}
}

func TestCurrentDSLFeatureSurfaceIsRegistered(t *testing.T) {
	def := Definition{Name: "all-features", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle:      "example",
		DisplayName: "All features",
		Triggers: []apiv1.Trigger{
			{Type: apiv1.TriggerManual},
			{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"ready": "true"}},
			{Type: apiv1.TriggerSchedule, Schedule: "@hourly"},
			{Type: apiv1.TriggerSignal, Signal: "done"},
		},
		Readiness: apiv1.ReadinessConditions{
			MaxConcurrentRuns: 1,
			MaxRunsPerHour:    2,
			MaxRunsPerDay:     3,
			MaxChainDepth:     4,
			MaxOpenPRs:        5,
		},
		Start: "agent-fail",
		Tasks: []apiv1.Task{
			{
				Name: "agent-fail", Type: apiv1.TaskAgentic, Goal: "agent",
				Goober: "coder", Inputs: map[string]string{"x": "y"},
				Capabilities: []string{"repo:push"}, Retry: &apiv1.RetryPolicy{MaxAttempts: 2, BackoffSeconds: 3},
				OnTimeout: apiv1.TaskOnTimeoutFail, ExpectedOutputs: []string{"result"}, Next: "agent-salvage",
			},
			{
				Name: "agent-salvage", Type: apiv1.TaskAgentic, Goal: "salvage",
				Goober: "coder", OnTimeout: apiv1.TaskOnTimeoutSalvage, Next: "shell-repo",
			},
			{
				Name: "shell-repo", Type: apiv1.TaskDeterministic, Goal: "shell",
				Run: &apiv1.DeterministicRun{
					Command: []string{"true"}, Image: "example/image",
					Network: apiv1.NetworkNone, Workspace: apiv1.WorkspaceRepo,
				},
				Inputs:     map[string]string{"kind": "shell", "resultFile": "result.json"},
				InputsFrom: map[string]string{"input": "output"}, Next: "shell-scratch",
			},
			{
				Name: "shell-scratch", Type: apiv1.TaskDeterministic, Goal: "scratch",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: "ci-poll",
			},
			{
				Name: "ci-poll", Type: apiv1.TaskDeterministic, Goal: "poll",
				Run:    &apiv1.DeterministicRun{Command: []string{"false"}},
				Inputs: map[string]string{"kind": "ci-poll"}, Next: "status-equals",
			},
		},
		Gates: []apiv1.Gate{
			automatedFeatureGate("status-equals", "output-equals"),
			automatedFeatureGate("output-equals", "output-not-equals"),
			automatedFeatureGate("output-not-equals", "output-numeric-gte"),
			automatedFeatureGate("output-numeric-gte", "output-numeric-lte"),
			automatedFeatureGate("output-numeric-lte", "output-numeric-lt"),
			automatedFeatureGate("output-numeric-lt", "output-matches"),
			automatedFeatureGate("output-matches", "ci-status"),
			automatedFeatureGate("ci-status", "land-outcome"),
			automatedFeatureGate("land-outcome", "queue-outcome"),
			automatedFeatureGate("queue-outcome", "agentic"),
			{
				Name: "agentic", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{"pass": "human-remind", "fail": TargetAbort, "needs-changes": TargetEscalate},
			},
			humanFeatureGate("human-remind", "remind", "human-escalate"),
			humanFeatureGate("human-escalate", "escalate", "human-reject"),
			humanFeatureGate("human-reject", "reject", TerminalComplete),
		},
	}}
	goober := apiv1.GooberSpec{
		Gaggle: "example", Role: "coder", DisplayName: "Coder", Instructions: "instructions.md",
		Harness: apiv1.HarnessCopilot, Model: "claude-sonnet-5",
		HarnessOptions: map[string]apiextensionsv1.JSON{"effort": {Raw: []byte(`"high"`)}},
		Capabilities:   []string{"repo:push"}, Skills: []string{"go"}, Tools: []string{"shell"},
		ScaleFactor: 2, Workflows: []string{"all-features"},
	}

	workflowFeatures, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	gooberFeatures, err := FeaturesForGoober(goober)
	if err != nil {
		t.Fatalf("FeaturesForGoober: %v", err)
	}
	got := featureIDs(append(workflowFeatures, gooberFeatures...))
	want := featureIDs(AllFeatures())
	if !slices.Equal(got, want) {
		t.Fatalf("resolved feature surface differs from registry\nmissing: %v\nextra: %v", difference(want, got), difference(got, want))
	}
}

func TestFeaturesForWorkflowResolvesImplicitDefaults(t *testing.T) {
	def := Definition{Name: "defaults", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle: "example",
		Start:  "agent",
		Tasks: []apiv1.Task{
			{Name: "agent", Type: apiv1.TaskAgentic, Goal: "agent", Goober: "coder", Next: "shell"},
			{Name: "shell", Type: apiv1.TaskDeterministic, Goal: "shell", Run: &apiv1.DeterministicRun{
				Command: []string{"true"},
			}},
		},
	}}

	features, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	got := featureIDs(features)
	for _, want := range []FeatureID{
		featureWorkflowReadiness,
		featureWorkflowMaxConcurrentRuns,
		featureWorkflowMaxRunsPerHour,
		featureTaskTimeoutFail,
		featureStageWorkspaceRepo,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("resolved features do not contain implicit default %q", want)
		}
	}
	for _, unwanted := range []FeatureID{featureTaskTimeoutSalvage, featureStageWorkspaceScratch} {
		if slices.Contains(got, unwanted) {
			t.Errorf("resolved features unexpectedly contain %q", unwanted)
		}
	}
}

func TestFeaturesForWorkflowOmitsAgenticTimeoutDefaultForDeterministicTasks(t *testing.T) {
	def := Definition{Name: "deterministic", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle: "example",
		Start:  "shell",
		Tasks: []apiv1.Task{{
			Name: "shell", Type: apiv1.TaskDeterministic, Goal: "shell",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}},
		}},
	}}

	features, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	if slices.Contains(featureIDs(features), featureTaskTimeoutFail) {
		t.Errorf("resolved deterministic-only workflow unexpectedly contains %q", featureTaskTimeoutFail)
	}
}

func TestFeaturesForGooberResolvesImplicitDefaults(t *testing.T) {
	features, err := FeaturesForGoober(apiv1.GooberSpec{
		Gaggle:       "example",
		Role:         "coder",
		Instructions: "instructions.md",
	})
	if err != nil {
		t.Fatalf("FeaturesForGoober: %v", err)
	}
	got := featureIDs(features)
	if !slices.Contains(got, featureGooberScaleFactor) {
		t.Errorf("resolved features do not contain implicit default %q", featureGooberScaleFactor)
	}
}

func TestCompileConsumesFeatureRegistry(t *testing.T) {
	all := AllFeatures()
	filtered := make([]Feature, 0, len(all)-1)
	for _, feature := range all {
		if feature.ID != featureWorkflowGaggle {
			filtered = append(filtered, feature)
		}
	}
	registry, err := NewFeatureRegistry(filtered)
	if err != nil {
		t.Fatalf("NewFeatureRegistry: %v", err)
	}
	original := currentFeatureRegistry
	currentFeatureRegistry = registry
	t.Cleanup(func() { currentFeatureRegistry = original })

	_, err = Compile(Definition{Name: "linear", Version: 1, Spec: linearSpec()})
	if err == nil || !strings.Contains(err.Error(), `DSL feature registry is missing: workflow.spec.gaggle`) {
		t.Fatalf("Compile error = %v, want missing registry feature", err)
	}
}

func automatedFeatureGate(check, next string) apiv1.Gate {
	return apiv1.Gate{
		Name: check, Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: check, Params: map[string]string{"key": "value"}},
		Branches:  map[string]string{"pass": next, "fail": TargetAbort, BranchEscalate: TargetEscalate},
	}
}

func humanFeatureGate(name, onTimeout, next string) apiv1.Gate {
	return apiv1.Gate{
		Name: name, Evaluator: apiv1.EvaluatorHuman,
		Human:    &apiv1.HumanGate{Approvers: []string{"maintainers"}, TimeoutSeconds: 1, OnTimeout: onTimeout},
		Branches: map[string]string{"pass": next, "fail": TargetAbort},
	}
}

func featureIDs(features []Feature) []FeatureID {
	ids := make([]FeatureID, 0, len(features))
	for _, feature := range features {
		ids = append(ids, feature.ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func difference(left, right []FeatureID) []FeatureID {
	var diff []FeatureID
	for _, id := range left {
		if !slices.Contains(right, id) {
			diff = append(diff, id)
		}
	}
	return diff
}
