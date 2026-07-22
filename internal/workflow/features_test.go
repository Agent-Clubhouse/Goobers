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
		"trigger.webhook",
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

func TestFeatureSupportDiagnostics(t *testing.T) {
	tests := []struct {
		name         string
		feature      Feature
		allowPreview bool
		wantCount    int
		wantBlocking bool
		wantParts    []string
	}{
		{
			name:      "ga",
			feature:   Feature{ID: "stable", Level: SupportGA, SinceVersion: "v1.0.0"},
			wantCount: 0,
		},
		{
			name:         "preview",
			feature:      Feature{ID: "new-field", Level: SupportPreview, SinceVersion: "v1.2.0"},
			allowPreview: true,
			wantCount:    1,
			wantParts:    []string{"new-field", "preview", "v1.2.0"},
		},
		{
			name: "deprecated",
			feature: Feature{
				ID:                   "old-field",
				Level:                SupportDeprecated,
				SinceVersion:         "v1.3.0",
				Replacement:          "new-field",
				RemovalTargetVersion: "v2.0.0",
			},
			wantCount: 1,
			wantParts: []string{"old-field", "new-field", "v2.0.0"},
		},
		{
			name: "removed",
			feature: Feature{
				ID:                    "removed-field",
				Level:                 SupportRemoved,
				SinceVersion:          "v2.0.0",
				LastSupportingVersion: "v1.9.0",
			},
			wantCount:    1,
			wantBlocking: true,
			wantParts:    []string{"removed-field", "v1.9.0"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diagnostics := CheckFeatureSupport([]Feature{tc.feature}, tc.allowPreview)
			if len(diagnostics) != tc.wantCount {
				t.Fatalf("diagnostics = %+v, want %d", diagnostics, tc.wantCount)
			}
			if tc.wantCount == 0 {
				return
			}
			if diagnostics[0].Blocking != tc.wantBlocking {
				t.Errorf("Blocking = %v, want %v", diagnostics[0].Blocking, tc.wantBlocking)
			}
			for _, want := range tc.wantParts {
				if !strings.Contains(diagnostics[0].Message, want) {
					t.Errorf("Message = %q, want it to contain %q", diagnostics[0].Message, want)
				}
			}
		})
	}
}

func TestPreviewFeatureRequiresOptIn(t *testing.T) {
	feature := Feature{ID: "new-field", Level: SupportPreview, SinceVersion: "v1.2.0"}
	diagnostics := CheckFeatureSupport([]Feature{feature}, false)
	if len(diagnostics) != 1 || !diagnostics[0].Blocking ||
		!strings.Contains(diagnostics[0].Message, PreviewFeaturesAnnotation) {
		t.Fatalf("diagnostics = %+v, want a blocking diagnostic naming the opt-in annotation", diagnostics)
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
		wantHistory := []SupportTransition{{
			Level:        SupportPreview,
			SinceVersion: initialFeatureSinceVersion,
		}}
		if !slices.Equal(feature.History, wantHistory) {
			t.Errorf("feature %q history = %+v, want %+v", feature.ID, feature.History, wantHistory)
		}
	}
}

func TestCurrentFeatureRegistrySatisfiesCompatibilityPolicy(t *testing.T) {
	if _, err := newFeatureRegistryAgainstReleased(
		latestReleasedFeatureRegistry,
		currentFeatures(initialFeatureSinceVersion),
	); err != nil {
		t.Fatalf("current feature registry violates compatibility policy: %v", err)
	}
}

func TestFeatureRegistryCompatibilityPolicyUsesReleasedSnapshot(t *testing.T) {
	transition := func(level SupportLevel, version string) SupportTransition {
		return SupportTransition{Level: level, SinceVersion: version}
	}
	feature := func(level SupportLevel, version string, history ...SupportTransition) Feature {
		return Feature{
			ID:           "example.feature",
			Level:        level,
			SinceVersion: version,
			History:      history,
		}
	}
	registry := func(features ...Feature) FeatureRegistry {
		t.Helper()
		result, err := NewFeatureRegistry(features)
		if err != nil {
			t.Fatalf("NewFeatureRegistry: %v", err)
		}
		return result
	}

	releasedGA := registry(feature(
		SupportGA,
		"v1.1.0",
		transition(SupportPreview, "dev"),
		transition(SupportGA, "v1.1.0"),
	))
	deprecatedAndRemoved := feature(
		SupportRemoved,
		"v1.3.0",
		transition(SupportPreview, "dev"),
		transition(SupportGA, "v1.1.0"),
		transition(SupportDeprecated, "v1.2.0"),
		transition(SupportRemoved, "v1.3.0"),
	)
	if _, err := newFeatureRegistryAgainstReleased(registry(), []Feature{deprecatedAndRemoved}); err == nil ||
		!strings.Contains(err.Error(), "must be deprecated in the latest released registry") {
		t.Fatalf("unreleased removal error = %v, want released-deprecation failure", err)
	}
	if _, err := newFeatureRegistryAgainstReleased(releasedGA, []Feature{deprecatedAndRemoved}); err == nil ||
		!strings.Contains(err.Error(), "must be deprecated in the latest released registry") {
		t.Fatalf("same-change deprecation and removal error = %v, want released-deprecation failure", err)
	}

	releasedDeprecated := registry(feature(
		SupportDeprecated,
		"v1.2.0",
		deprecatedAndRemoved.History[:3]...,
	))
	if _, err := newFeatureRegistryAgainstReleased(releasedDeprecated, []Feature{deprecatedAndRemoved}); err != nil {
		t.Fatalf("removal after a released deprecated minor was rejected: %v", err)
	}
}

func TestFeatureRegistryCompatibilityPolicyPreservesReleasedSnapshot(t *testing.T) {
	transition := func(level SupportLevel, version string) SupportTransition {
		return SupportTransition{Level: level, SinceVersion: version}
	}
	releasedFeature := Feature{
		ID:           "example.feature",
		Level:        SupportGA,
		SinceVersion: "v1.1.0",
		History: []SupportTransition{
			transition(SupportPreview, "dev"),
			transition(SupportGA, "v1.1.0"),
		},
	}
	released, err := NewFeatureRegistry([]Feature{releasedFeature})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		candidates []Feature
		want       string
	}{
		{
			name: "released feature omitted",
			want: "must remain in the registry",
		},
		{
			name: "released history rewritten",
			candidates: []Feature{{
				ID:           "example.feature",
				Level:        SupportGA,
				SinceVersion: "v1.2.0",
				History: []SupportTransition{
					transition(SupportPreview, "dev"),
					transition(SupportGA, "v1.2.0"),
				},
			}},
			want: "lifecycle history must not change",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newFeatureRegistryAgainstReleased(released, test.candidates)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newFeatureRegistryAgainstReleased() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestFeatureRegistryCompatibilityPolicy(t *testing.T) {
	transition := func(level SupportLevel, version string) SupportTransition {
		return SupportTransition{Level: level, SinceVersion: version}
	}
	valid := Feature{
		ID:           "example.feature",
		Level:        SupportRemoved,
		SinceVersion: "v1.3.0",
		History: []SupportTransition{
			transition(SupportPreview, "dev"),
			transition(SupportGA, "v1.1.0"),
			transition(SupportDeprecated, "v1.2.0"),
			transition(SupportRemoved, "v1.3.0"),
		},
	}
	if _, err := NewFeatureRegistry([]Feature{valid}); err != nil {
		t.Fatalf("valid lifecycle rejected: %v", err)
	}

	tests := []struct {
		name    string
		feature Feature
		want    string
	}{
		{
			name: "missing history",
			feature: Feature{
				ID:           "example.feature",
				Level:        SupportGA,
				SinceVersion: "v1.0.0",
			},
			want: "history must not be empty",
		},
		{
			name: "lifecycle starts deprecated",
			feature: Feature{
				ID:           "example.feature",
				Level:        SupportDeprecated,
				SinceVersion: "v1.0.0",
				History: []SupportTransition{
					transition(SupportDeprecated, "v1.0.0"),
				},
			},
			want: "lifecycle must start at preview or ga",
		},
		{
			name: "initial version is not a release",
			feature: Feature{
				ID:           "example.feature",
				Level:        SupportGA,
				SinceVersion: "1.0.0",
				History: []SupportTransition{
					transition(SupportGA, "1.0.0"),
				},
			},
			want: "must use vMAJOR.MINOR.PATCH",
		},
		{
			name: "ga directly to removed",
			feature: Feature{
				ID:           "example.feature",
				Level:        SupportRemoved,
				SinceVersion: "v1.1.0",
				History: []SupportTransition{
					transition(SupportGA, "v1.0.0"),
					transition(SupportRemoved, "v1.1.0"),
				},
			},
			want: `invalid lifecycle transition "ga" -> "removed"`,
		},
		{
			name: "preview directly to removed",
			feature: Feature{
				ID:           "example.feature",
				Level:        SupportRemoved,
				SinceVersion: "v1.1.0",
				History: []SupportTransition{
					transition(SupportPreview, "v1.0.0"),
					transition(SupportRemoved, "v1.1.0"),
				},
			},
			want: `invalid lifecycle transition "preview" -> "removed"`,
		},
		{
			name: "removed within deprecation minor",
			feature: Feature{
				ID:           "example.feature",
				Level:        SupportRemoved,
				SinceVersion: "v1.2.4",
				History: []SupportTransition{
					transition(SupportGA, "v1.0.0"),
					transition(SupportDeprecated, "v1.2.0"),
					transition(SupportRemoved, "v1.2.4"),
				},
			},
			want: "must remain deprecated until a later minor release",
		},
		{
			name: "versions out of order",
			feature: Feature{
				ID:           "example.feature",
				Level:        SupportDeprecated,
				SinceVersion: "v1.1.0",
				History: []SupportTransition{
					transition(SupportGA, "v1.2.0"),
					transition(SupportDeprecated, "v1.1.0"),
				},
			},
			want: `lifecycle version "v1.1.0" must follow "v1.2.0"`,
		},
		{
			name: "current state differs from history",
			feature: Feature{
				ID:           "example.feature",
				Level:        SupportGA,
				SinceVersion: "v1.1.0",
				History: []SupportTransition{
					transition(SupportPreview, "v1.0.0"),
				},
			},
			want: "does not match lifecycle history",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewFeatureRegistry([]Feature{test.feature})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewFeatureRegistry() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestFeatureRegistryLifecycleHistoryIsImmutable(t *testing.T) {
	features := []Feature{{
		ID:           "example.feature",
		Level:        SupportPreview,
		SinceVersion: "dev",
		History: []SupportTransition{{
			Level:        SupportPreview,
			SinceVersion: "dev",
		}},
	}}
	registry, err := NewFeatureRegistry(features)
	if err != nil {
		t.Fatal(err)
	}
	features[0].History[0].Level = SupportRemoved
	lookedUp, ok := registry.Lookup("example.feature")
	if !ok {
		t.Fatal("registered feature was not found")
	}
	lookedUp.History[0].Level = SupportRemoved
	lookedUpAgain, _ := registry.Lookup("example.feature")
	if lookedUpAgain.History[0].Level != SupportPreview {
		t.Fatalf("registry history was mutated through a returned feature: %+v", lookedUpAgain.History)
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
			{Type: apiv1.TriggerWebhook, Events: []string{"issues"}},
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
				TimeoutSeconds: 30, Limits: &apiv1.Limits{MaxDurationSeconds: 30, MaxTokens: 1000, MaxCostUSD: 1},
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
					Env: map[string]string{"CI": "true"}, Network: apiv1.NetworkNone,
					Workspace: apiv1.WorkspaceRepo, SyncBase: true,
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
				Inputs: map[string]string{"kind": "ci-poll"}, ContinueOnError: true, Next: "status-equals",
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
				Agentic: &apiv1.AgenticGate{
					Goober: "reviewer", TimeoutSeconds: 30,
					Retry: &apiv1.RetryPolicy{MaxAttempts: 2, BackoffSeconds: 3},
				},
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
		TimeoutSeconds: 3600,
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
	want := expectedCurrentDSLFeatureIDs()
	if !slices.Equal(got, want) {
		t.Fatalf("resolved feature surface differs from current DSL\nmissing: %v\nextra: %v", difference(want, got), difference(got, want))
	}
	registered := featureIDs(AllFeatures())
	if !slices.Equal(registered, want) {
		t.Fatalf("registered feature surface differs from current DSL\nmissing: %v\nextra: %v", difference(want, registered), difference(registered, want))
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

// TestFeaturesForGooberTimeoutSeconds pins that the goober-level default
// timeout (#1070) is a recognized, resolvable feature only when set — an
// unset field must not pull it into the surface (mirroring model/harnessOptions).
func TestFeaturesForGooberTimeoutSeconds(t *testing.T) {
	base := apiv1.GooberSpec{Gaggle: "example", Role: "coder", Instructions: "instructions.md"}
	if features, err := FeaturesForGoober(base); err != nil {
		t.Fatalf("FeaturesForGoober (unset): %v", err)
	} else if slices.Contains(featureIDs(features), featureGooberTimeoutSeconds) {
		t.Errorf("unset TimeoutSeconds must not surface %q", featureGooberTimeoutSeconds)
	}

	withTimeout := base
	withTimeout.TimeoutSeconds = 3600
	features, err := FeaturesForGoober(withTimeout)
	if err != nil {
		t.Fatalf("FeaturesForGoober (set): %v", err)
	}
	if !slices.Contains(featureIDs(features), featureGooberTimeoutSeconds) {
		t.Errorf("set TimeoutSeconds must surface %q", featureGooberTimeoutSeconds)
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

	_, err = compileAcknowledged(Definition{Name: "linear", Version: 1, Spec: linearSpec()})
	if err == nil || !strings.Contains(err.Error(), `DSL feature registry is missing: workflow.spec.gaggle`) {
		t.Fatalf("Compile error = %v, want missing registry feature", err)
	}
}

func automatedFeatureGate(check, next string) apiv1.Gate {
	return apiv1.Gate{
		Name: check, Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{
			Check: check, Params: map[string]string{"key": "value"}, TimeoutSeconds: 30,
			Retry: &apiv1.RetryPolicy{MaxAttempts: 2, BackoffSeconds: 3}, PollIntervalSeconds: 5,
		},
		Branches: map[string]string{"pass": next, "fail": TargetAbort, BranchEscalate: TargetEscalate},
	}
}

func humanFeatureGate(name, onTimeout, next string) apiv1.Gate {
	return apiv1.Gate{
		Name: name, Evaluator: apiv1.EvaluatorHuman,
		Human:    &apiv1.HumanGate{Approvers: []string{"maintainers"}, TimeoutSeconds: 1, OnTimeout: onTimeout},
		Branches: map[string]string{"pass": next, "fail": TargetAbort},
	}
}

func expectedCurrentDSLFeatureIDs() []FeatureID {
	ids := []FeatureID{
		"workflow.spec.gaggle",
		"workflow.spec.displayName",
		"workflow.spec.triggers",
		"workflow.spec.readiness",
		"workflow.spec.readiness.maxConcurrentRuns",
		"workflow.spec.readiness.maxRunsPerHour",
		"workflow.spec.readiness.maxRunsPerDay",
		"workflow.spec.readiness.maxChainDepth",
		"workflow.spec.readiness.maxOpenPRs",
		"workflow.spec.start",
		"workflow.spec.tasks",
		"workflow.spec.gates",
		"workflow.terminal.complete",
		"workflow.terminal.abort",
		"workflow.terminal.escalate",
		"goober.spec.gaggle",
		"goober.spec.role",
		"goober.spec.displayName",
		"goober.spec.instructions",
		"goober.spec.harness.copilot",
		"goober.spec.model",
		"goober.spec.harnessOptions",
		"goober.spec.timeoutSeconds",
		"goober.spec.capabilities",
		"goober.spec.skills",
		"goober.spec.tools",
		"goober.spec.scaleFactor",
		"goober.spec.workflows",
		"trigger.manual",
		"trigger.backlog-item",
		"trigger.backlog-item.selector",
		"trigger.schedule",
		"trigger.signal",
		"trigger.webhook",
		"task.name",
		"task.deterministic",
		"task.agentic",
		"task.goal",
		"task.goober",
		"task.inputs",
		"task.inputsFrom",
		"task.capabilities",
		"task.retry",
		"task.retry.maxAttempts",
		"task.retry.backoff",
		"task.timeoutSeconds",
		"task.limits",
		"task.limits.maxDurationSeconds",
		"task.limits.maxTokens",
		"task.limits.maxCostUSD",
		"task.onTimeout.fail",
		"task.onTimeout.salvage",
		"task.expectedOutputs",
		"task.continueOnError",
		"task.next",
		"stage.shell",
		"stage.ci-poll",
		"stage.run.command",
		"stage.run.env",
		"stage.run.image",
		"stage.run.network.none",
		"stage.run.syncBase",
		"stage.run.workspace.repo",
		"stage.run.workspace.scratch",
		"stage.resultFile",
		"gate.name",
		"gate.branches",
		"gate.branch.escalate",
		"gate.evaluator.automated",
		"gate.evaluator.automated.check",
		"gate.evaluator.automated.params",
		"gate.evaluator.automated.timeoutSeconds",
		"gate.evaluator.automated.retry",
		"gate.evaluator.automated.retry.maxAttempts",
		"gate.evaluator.automated.retry.backoff",
		"gate.evaluator.automated.pollIntervalSeconds",
		"gate.evaluator.automated.check.status-equals",
		"gate.evaluator.automated.check.output-equals",
		"gate.evaluator.automated.check.output-not-equals",
		"gate.evaluator.automated.check.output-numeric-gte",
		"gate.evaluator.automated.check.output-numeric-lte",
		"gate.evaluator.automated.check.output-numeric-lt",
		"gate.evaluator.automated.check.output-matches",
		"gate.evaluator.automated.check.ci-status",
		"gate.evaluator.automated.check.land-outcome",
		"gate.evaluator.automated.check.queue-outcome",
		"gate.evaluator.agentic",
		"gate.evaluator.agentic.goober",
		"gate.evaluator.agentic.timeoutSeconds",
		"gate.evaluator.agentic.retry",
		"gate.evaluator.agentic.retry.maxAttempts",
		"gate.evaluator.agentic.retry.backoff",
		"gate.evaluator.human",
		"gate.evaluator.human.approvers",
		"gate.evaluator.human.timeout",
		"gate.evaluator.human.onTimeout.remind",
		"gate.evaluator.human.onTimeout.escalate",
		"gate.evaluator.human.onTimeout.reject",
	}
	slices.Sort(ids)
	return ids
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
