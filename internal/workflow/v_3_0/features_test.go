package v30

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
		"stage.run.script",
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

// TestCurrentFeatureClassification pins the 3.0 registry's classification:
// the carried-forward canonical surface is GA with its historical "dev"
// since-version, the 2.0-promoted stage-qualified inputsFrom keeps its
// promotion history, and the 3.0-only surface is GA since v0.4.0 — the
// version-level PREVIEW gate (DVL010/DVL011) is the opt-in that guards the
// whole 3.0 surface, so no per-feature preview level exists in this registry
// (the sole 2.0 preview feature, gaggle.spec.sandbox, does not carry into
// 3.0 — dsl-3.0.md D15).
func TestCurrentFeatureClassification(t *testing.T) {
	features := AllFeatures()
	if len(features) == 0 {
		t.Fatal("feature registry is empty")
	}
	v30Seen := 0
	for _, feature := range features {
		wantLevel := SupportGA
		wantSince := initialFeatureSinceVersion
		wantHistory := []SupportTransition{{Level: SupportGA, SinceVersion: initialFeatureSinceVersion}}
		if introduced, isNew := v30Introductions[feature.ID]; isNew {
			wantSince = introduced
			wantHistory = []SupportTransition{{Level: SupportGA, SinceVersion: introduced}}
			v30Seen++
		}
		if feature.ID == featureTaskInputsFromQualified {
			// Promoted preview -> ga by the 2.0 lock (#3292): the released
			// preview baseline stays in history and the ga transition is
			// pinned to the promoting release, not the "dev" baseline.
			wantSince = "v0.2.0"
			wantHistory = []SupportTransition{
				{Level: SupportPreview, SinceVersion: initialFeatureSinceVersion},
				{Level: SupportGA, SinceVersion: "v0.2.0"},
			}
		}
		if feature.Level != wantLevel {
			t.Errorf("feature %q level = %q, want %q", feature.ID, feature.Level, wantLevel)
		}
		if feature.SinceVersion != wantSince {
			t.Errorf("feature %q since-version = %q, want %q", feature.ID, feature.SinceVersion, wantSince)
		}
		if len(feature.DSLVersions) != 1 ||
			feature.DSLVersions[0] != (DSLFeatureSupport{Version: DSLVersion, Level: wantLevel}) {
			t.Errorf("feature %q DSL versions = %+v, want level %q", feature.ID, feature.DSLVersions, wantLevel)
		}
		if !slices.Equal(feature.History, wantHistory) {
			t.Errorf("feature %q history = %+v, want %+v", feature.ID, feature.History, wantHistory)
		}
	}
	if v30Seen != len(v30Introductions) {
		t.Errorf("registry carries %d of the %d 3.0-introduced features", v30Seen, len(v30Introductions))
	}
	// The removed 2.0 surface must not resolve in this registry at all.
	for _, removed := range []FeatureID{
		"gaggle.spec.sandbox", "task.requiredCapabilities",
		"gaggle.spec.requiredCapabilities", "stage.run.network.none",
	} {
		if _, ok := LookupFeature(removed); ok {
			t.Errorf("removed 2.0 feature %q must not exist in the 3.0 registry", removed)
		}
	}
}

// TestStandardFeaturesAreGA is the field-level regression for #1196: the
// standard fields the issue reported as wrongly flagged must be GA.
func TestStandardFeaturesAreGA(t *testing.T) {
	for _, id := range []FeatureID{
		featureTaskAgentic, featureGooberRole, featureGooberCapabilities,
		featureWorkflowTriggers, featureStageShell, featureTaskRetry, featureStageScript,
	} {
		feature, ok := LookupFeature(id)
		if !ok {
			t.Errorf("feature %q missing from registry", id)
			continue
		}
		if feature.Level != SupportGA {
			t.Errorf("standard feature %q level = %q, want GA (#1196)", id, feature.Level)
		}
	}
}

func TestFeaturesForWorkflowIncludesContractFields(t *testing.T) {
	def := Definition{
		DSLVersion: DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:      "test",
			Triggers:    []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem, TrustLabel: "approved", LabelPredicate: `"ready" in labels`, FieldPredicate: `fields["number"] > 0`}},
			RunControls: &apiv1.RunControls{MaxRepasses: 2},
			Start:       "query",
			Tasks: []apiv1.Task{{
				Name:             "query",
				Type:             apiv1.TaskDeterministic,
				Goal:             "query backlog",
				Inputs:           map[string]string{"fieldOrder": "number:asc"},
				MinimumIntegrity: apiv1.IntegrityMaintainer,
				ContextFrom:      []string{"claim"},
				PolicyActions:    []string{"claim-item"},
			}},
		},
	}
	features, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	got := featureIDs(features)
	for _, id := range []FeatureID{
		featureWorkflowRunControls,
		featureTriggerBacklogItemTrustLabel,
		featureTriggerLabelPredicate,
		featureTriggerFieldPredicate,
		featureTaskInputFieldOrder,
		featureTaskMinimumIntegrity,
		featureTaskContextFrom,
		featureTaskPolicyActions,
	} {
		if !slices.Contains(got, id) {
			t.Errorf("FeaturesForWorkflow omitted %q: %v", id, got)
		}
	}
}

func TestFeaturesAtDSLVersion(t *testing.T) {
	features, err := FeaturesAtDSLVersion(AllFeatures(), DSLVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(features) != len(AllFeatures()) {
		t.Fatalf("features for interpreter DSL version = %d, want %d", len(features), len(AllFeatures()))
	}
}

func TestCurrentFeatureRegistrySatisfiesLifecyclePolicy(t *testing.T) {
	if _, err := NewFeatureRegistry(currentFeatures(initialFeatureSinceVersion)); err != nil {
		t.Fatalf("current feature registry violates lifecycle policy: %v", err)
	}
}

func TestFeatureRegistryCompatibilityPolicyUsesReleasedSnapshot(t *testing.T) {
	transition := func(level SupportLevel, version string) SupportTransition {
		return SupportTransition{Level: level, SinceVersion: version}
	}
	feature := func(level SupportLevel, version string, history ...SupportTransition) Feature {
		result := Feature{
			ID:           "example.feature",
			Level:        level,
			SinceVersion: version,
			History:      history,
		}
		switch level {
		case SupportDeprecated:
			result.Replacement = "replacement.feature"
			result.RemovalTargetVersion = "v1.3.0"
		case SupportRemoved:
			result.LastSupportingVersion = "v1.2.0"
		}
		return result
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

// TestFeatureRegistryCompatibilityPolicyPinsReleasedDSLVersions is the
// failing-direction guard for the #3292 append-only per-version rule:
// validateFeatureRegistryEvolution pins lifecycle History, but before this
// guard a released feature could silently drop a DSL version from DSLVersions
// or regress that version's level.
func TestFeatureRegistryCompatibilityPolicyPinsReleasedDSLVersions(t *testing.T) {
	transition := func(level SupportLevel, version string) SupportTransition {
		return SupportTransition{Level: level, SinceVersion: version}
	}
	history := []SupportTransition{
		transition(SupportPreview, "dev"),
		transition(SupportGA, "v1.1.0"),
	}
	feature := func(id FeatureID, versions ...DSLFeatureSupport) Feature {
		return Feature{
			ID:           id,
			Level:        SupportGA,
			SinceVersion: "v1.1.0",
			DSLVersions:  versions,
			History:      slices.Clone(history),
		}
	}
	bothVersions := []DSLFeatureSupport{
		{Version: "1.4", Level: SupportGA},
		{Version: "2.0", Level: SupportGA},
	}
	// A sibling keeps every released version declared, so the versions stay in
	// the current registry's domain and the accident under test is one feature
	// losing (or regressing) a version its siblings keep.
	sibling := feature("sibling.feature", bothVersions...)
	released, err := NewFeatureRegistry([]Feature{
		feature("example.feature", bothVersions...),
		sibling,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		candidate Feature
		want      string
	}{
		{
			name:      "released DSL version dropped",
			candidate: feature("example.feature", DSLFeatureSupport{Version: "2.0", Level: SupportGA}),
			want:      `released DSL feature "example.feature" must remain available at DSL version "1.4"`,
		},
		{
			name: "released DSL version level regressed",
			candidate: feature("example.feature",
				DSLFeatureSupport{Version: "1.4", Level: SupportPreview},
				DSLFeatureSupport{Version: "2.0", Level: SupportGA},
			),
			want: `at DSL version "1.4" may not move from "ga" to "preview"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newFeatureRegistryAgainstReleased(released, []Feature{test.candidate, sibling})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newFeatureRegistryAgainstReleased() error = %v, want containing %q", err, test.want)
			}
		})
	}

	t.Run("valid transition and appended version", func(t *testing.T) {
		releasedNarrow, err := NewFeatureRegistry([]Feature{feature(
			"example.feature",
			DSLFeatureSupport{Version: "1.4", Level: SupportGA},
		)})
		if err != nil {
			t.Fatal(err)
		}
		// 1.4 advancing ga -> deprecated follows the lifecycle transition
		// rules, and declaring the feature in 2.0 is an append, not a change.
		candidate := feature("example.feature",
			DSLFeatureSupport{Version: "1.4", Level: SupportDeprecated},
			DSLFeatureSupport{Version: "2.0", Level: SupportGA},
		)
		if _, err := newFeatureRegistryAgainstReleased(releasedNarrow, []Feature{candidate}); err != nil {
			t.Fatalf("append-only-compatible candidate was rejected: %v", err)
		}
	})

	t.Run("interpreter-scoped version domains stay comparable", func(t *testing.T) {
		// Released snapshots come from the merged cross-interpreter registry,
		// so they declare versions (1.4) this interpreter never serves. A
		// version absent from the current registry's whole domain is out of
		// frame — whole-version retirement belongs to the support matrix.
		nextOnly := []Feature{
			feature("example.feature", DSLFeatureSupport{Version: "2.0", Level: SupportGA}),
			feature("sibling.feature", DSLFeatureSupport{Version: "2.0", Level: SupportGA}),
		}
		if _, err := newFeatureRegistryAgainstReleased(released, nextOnly); err != nil {
			t.Fatalf("interpreter-scoped registry was rejected: %v", err)
		}
	})
}

func TestFeatureRegistryCompatibilityPolicy(t *testing.T) {
	transition := func(level SupportLevel, version string) SupportTransition {
		return SupportTransition{Level: level, SinceVersion: version}
	}
	valid := Feature{
		ID:                    "example.feature",
		Level:                 SupportRemoved,
		SinceVersion:          "v1.3.0",
		LastSupportingVersion: "v1.2.0",
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
			{
				Type:           apiv1.TriggerBacklogItem,
				Selector:       map[string]string{"ready": "true"},
				TrustLabel:     "approved",
				LabelPredicate: `"ready" in labels`,
				FieldPredicate: `fields["number"] > 0`,
				Priority:       3,
			},
			{
				Type:     apiv1.TriggerSchedule,
				Schedule: "@hourly",
				IdleBackoff: &apiv1.IdleBackoff{
					Enabled: func() *bool { enabled := true; return &enabled }(),
					Floor:   "1m",
					Ceiling: "15m",
				},
			},
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
		RunControls: &apiv1.RunControls{
			MaxRepasses:       2,
			StalledRunTimeout: "30m",
			MaxRunDuration:    "4h",
		},
		OutboxMirrorPath: "/var/goobers/outbox",
		DocsRoots:        []string{"docs"},
		TutorScope:       &apiv1.TutorScope{Tier: apiv1.TutorScopePerWorkflow, Target: "all-features"},
		Requires:         &apiv1.WorkflowRequirements{Capabilities: []string{"pr.merge"}},
		Start:            "agent-fail",
		Tasks: []apiv1.Task{
			{
				Name: "agent-fail", Type: apiv1.TaskAgentic, Goal: "agent",
				Goober: "coder", Inputs: map[string]string{"x": "y", "fieldOrder": "number:asc"},
				Capabilities: []string{"repo:push"}, MinimumIntegrity: apiv1.IntegrityMaintainer,
				ContextFrom: []string{"claim"}, PolicyActions: []string{"claim-item"},
				RunsOn: &apiv1.RunsOn{
					OS: "linux", CPU: "2000m", Memory: "4Gi", Disk: "20Gi",
					Capabilities: []string{"dotnet@8"},
					Restrictions: []string{"network:allowlist"},
				},
				Outbox:           []string{"reports"},
				OutboxMirrorPath: "/var/goobers/task-outbox",
				Retry:            &apiv1.RetryPolicy{MaxAttempts: 2, BackoffSeconds: 3},
				TimeoutSeconds:   30, Limits: &apiv1.Limits{MaxDurationSeconds: 30, MaxTokens: 1000, MaxCostUSD: 1},
				OnTimeout: apiv1.TaskOnTimeoutFail, ExpectedOutputs: []string{"result"}, Next: "agent-salvage",
			},
			{
				Name: "agent-salvage", Type: apiv1.TaskAgentic, Goal: "salvage",
				Goober: "coder", OnTimeout: apiv1.TaskOnTimeoutSalvage,
				// The agentic seam: a task-level workspace, which an agentic
				// stage has no Run to express.
				Workspace: apiv1.WorkspaceRepoReadOnly,
				Next:      "shell-repo",
			},
			{
				Name: "shell-repo", Type: apiv1.TaskDeterministic, Goal: "shell",
				Run: &apiv1.DeterministicRun{
					Command: []string{"true"}, Env: map[string]string{"CI": "true"},
					Workspace: apiv1.WorkspaceRepo, SyncBase: true,
				},
				// The DSL 3.0 repo-handoff surface: a declared edge from the
				// producing agentic stage, and the committing-script opt-in.
				RepoFrom:    apiv1.RepoFrom{"agent-fail"},
				CommitsRepo: true,
				Inputs:      map[string]string{"kind": "shell", "resultFile": "result.json"},
				InputsFrom:  map[string]string{"input": "output", "qualified": "agent-fail.result"}, Next: "shell-scratch",
			},
			{
				Name: "shell-scratch", Type: apiv1.TaskDeterministic, Goal: "scratch",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: "shell-script",
			},
			{
				Name: "shell-script", Type: apiv1.TaskDeterministic, Goal: "inline",
				Run:  &apiv1.DeterministicRun{Script: "true"},
				Next: "ci-poll",
			},
			{
				Name: "ci-poll", Type: apiv1.TaskDeterministic, Goal: "poll",
				Run:    &apiv1.DeterministicRun{Command: []string{"false"}},
				Inputs: map[string]string{"kind": "ci-poll"}, ContinueOnError: true, Next: "external-telemetry",
			},
			{
				Name: "external-telemetry", Type: apiv1.TaskDeterministic, Goal: "query",
				Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "external-telemetry"}},
				Inputs: map[string]string{"kind": "external-telemetry"}, Next: "status-equals",
			},
		},
		Gates: []apiv1.Gate{
			automatedFeatureGate("status-equals", "failure-class"),
			automatedFeatureGate("failure-class", "output-equals"),
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
					Workspace: apiv1.WorkspaceRepoReadOnly,
					Retry:     &apiv1.RetryPolicy{MaxAttempts: 2, BackoffSeconds: 3},
				},
				Branches: map[string]string{"pass": "human-remind", "fail": TargetAbort, "needs-changes": TargetEscalate},
			},
			humanFeatureGate("human-remind", "remind", "human-escalate"),
			humanFeatureGate("human-escalate", "escalate", "human-reject"),
			humanFeatureGate("human-reject", "reject", TerminalComplete),
		},
		Parallels: []apiv1.Parallel{{
			Name:                  "fan",
			FailurePolicy:         apiv1.BranchAllOrNothing,
			Join:                  "collate",
			OnFailure:             TargetEscalate,
			BranchTimeoutSeconds:  900,
			MaxConcurrentBranches: 2,
			Branches: []apiv1.Branch{
				{Name: "a", Start: "agent-fail"},
				{Name: "b", Start: "agent-fail"},
			},
		}},
	}}
	goober := apiv1.GooberSpec{
		Gaggle: "example", Role: "coder", DisplayName: "Coder", Instructions: "instructions.md",
		Harness: apiv1.HarnessCopilot, Model: "claude-sonnet-5",
		HarnessOptions: map[string]apiextensionsv1.JSON{"effort": {Raw: []byte(`"high"`)}},
		TimeoutSeconds: 3600,
		Capabilities:   []string{"repo:push"}, Skills: []string{"go"}, Tools: []string{"shell"},
		MCPServers:               []apiv1.MCPServer{{Name: "context", Command: "context-mcp"}},
		PolicyActions:            []string{"claim-item"},
		ConditionalPolicyActions: []string{"merge-pr"},
		ScaleFactor:              2, Workflows: []string{"all-features"},
	}
	// One GaggleSpec carries one provider per site, and RepoRef.Project is
	// ADO-only while BaseURL is gitea-only — so the mutually exclusive enum
	// values need per-provider gaggle variants whose resolutions are unioned,
	// mirroring the goober/claudeGoober split below.
	githubGaggle := apiv1.GaggleSpec{
		DisplayName:  "Example",
		SelfIdentity: "goobers-bot",
		Project: apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub,
			Owner:    "acme",
			Name:     "app",
			Checkout: &apiv1.CheckoutSpec{Sparse: []string{"src"}},
		},
		Backlog: apiv1.BacklogRef{
			Provider:       apiv1.ProviderGitHub,
			Project:        "acme/app",
			Labels:         []string{"goobers:ready"},
			LabelPredicate: `"ready" in labels`,
			FieldPredicate: `fields["number"] > 0`,
		},
		Isolation: apiv1.GaggleIsolation{
			Namespace:   "gaggle-example",
			IdentityRef: "gaggle-example-identity",
		},
		AdditionalRepos: []apiv1.RepoRef{{
			Provider: apiv1.ProviderGitHub,
			Owner:    "acme",
			Name:     "docs",
			Checkout: &apiv1.CheckoutSpec{Sparse: []string{"docs"}},
		}},
		CICommand:       []string{"make", "ci"},
		BranchNamespace: "goobers/",
		RunsOn: &apiv1.GaggleRunsOn{
			OS:           "linux",
			Capabilities: []string{"dotnet@8"},
			Restrictions: []string{"network:allowlist"},
		},
		RunControls: &apiv1.RunControls{
			MaxRepasses:       2,
			StalledRunTimeout: "30m",
			MaxRunDuration:    "4h",
		},
		OutboxMirrorPath: "/var/goobers/outbox",
		Workcopies:       &apiv1.GaggleWorkcopies{Root: "/var/goobers/workcopies"},
		RequireLabels:    []string{"team:web"},
		Siblings: []apiv1.GaggleSibling{{
			Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "app"},
			Label:   "Billing team",
		}},
	}
	adoGaggle := apiv1.GaggleSpec{
		Project: apiv1.RepoRef{
			Provider: apiv1.ProviderADO,
			Owner:    "acme",
			Project:  "platform",
			Name:     "app",
		},
		Backlog: apiv1.BacklogRef{
			Provider: apiv1.ProviderADO,
			Project:  "platform",
		},
		AdditionalRepos: []apiv1.RepoRef{{
			Provider: apiv1.ProviderADO,
			Owner:    "acme",
			Project:  "platform",
			Name:     "docs",
		}},
	}
	giteaGaggle := apiv1.GaggleSpec{
		Project: apiv1.RepoRef{
			Provider: apiv1.ProviderGitea,
			BaseURL:  "https://gitea.example.com",
			Owner:    "acme",
			Name:     "app",
		},
		Backlog: apiv1.BacklogRef{
			Provider: apiv1.ProviderGitea,
			BaseURL:  "https://gitea.example.com",
			Project:  "acme/app",
		},
		AdditionalRepos: []apiv1.RepoRef{{
			Provider: apiv1.ProviderGitea,
			BaseURL:  "https://gitea.example.com",
			Owner:    "acme",
			Name:     "docs",
		}},
	}

	workflowFeatures, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	gooberFeatures, err := FeaturesForGoober(goober)
	if err != nil {
		t.Fatalf("FeaturesForGoober: %v", err)
	}
	claudeGoober := goober
	claudeGoober.Harness = apiv1.HarnessClaudeCode
	claudeGoober.Model = ""
	claudeGoober.HarnessOptions = nil
	claudeFeatures, err := FeaturesForGoober(claudeGoober)
	if err != nil {
		t.Fatalf("FeaturesForGoober (claude-code): %v", err)
	}
	var gaggleFeatures []Feature
	for name, gaggle := range map[string]apiv1.GaggleSpec{
		"github": githubGaggle,
		"ado":    adoGaggle,
		"gitea":  giteaGaggle,
	} {
		features, err := FeaturesForGaggle(gaggle)
		if err != nil {
			t.Fatalf("FeaturesForGaggle (%s): %v", name, err)
		}
		gaggleFeatures = append(gaggleFeatures, features...)
	}
	got := featureIDs(append(append(append(workflowFeatures, gooberFeatures...), claudeFeatures...), gaggleFeatures...))
	want := append(expectedCurrentDSLFeatureIDs(), gaggleOnlyFeatureIDs()...)
	slices.Sort(want)
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

	_, err = Compile(Definition{Name: "linear", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"},
		},
	}}, WithPreviewFeatures(true))
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
		MaxRepasses: 2,
		Branches:    map[string]string{"pass": next, "fail": TargetAbort, BranchEscalate: TargetEscalate},
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
		"workflow.spec.runControls",
		"workflow.spec.readiness.maxConcurrentRuns",
		"workflow.spec.readiness.maxRunsPerHour",
		"workflow.spec.readiness.maxRunsPerDay",
		"workflow.spec.readiness.maxChainDepth",
		"workflow.spec.readiness.maxOpenPRs",
		"workflow.spec.start",
		"workflow.spec.tasks",
		"workflow.spec.gates",
		"workflow.spec.parallels",
		"workflow.spec.parallels.failurePolicy",
		"workflow.spec.parallels.branches",
		"workflow.spec.parallels.join",
		"workflow.spec.parallels.onFailure",
		"workflow.spec.parallels.branchTimeoutSeconds",
		"workflow.spec.parallels.maxConcurrentBranches",
		"workflow.terminal.complete",
		"workflow.terminal.abort",
		"workflow.terminal.escalate",
		"goober.spec.gaggle",
		"goober.spec.role",
		"goober.spec.displayName",
		"goober.spec.instructions",
		"goober.spec.harness.claude-code",
		"goober.spec.harness.copilot",
		"goober.spec.model",
		"goober.spec.harnessOptions",
		"goober.spec.timeoutSeconds",
		"goober.spec.capabilities",
		"goober.spec.skills",
		"goober.spec.tools",
		"goober.spec.mcpServers",
		"goober.spec.scaleFactor",
		"goober.spec.workflows",
		"trigger.manual",
		"trigger.backlog-item",
		"trigger.backlog-item.selector",
		"trigger.backlog-item.trustLabel",
		"trigger.labelPredicate",
		"trigger.fieldPredicate",
		"trigger.schedule",
		"trigger.signal",
		"trigger.webhook",
		"task.name",
		"task.deterministic",
		"task.agentic",
		"task.goal",
		"task.goober",
		"task.inputs",
		"task.inputs.fieldOrder",
		"task.inputsFrom",
		"task.inputsFrom.stageQualified",
		"task.capabilities",
		"task.minimumIntegrity",
		"task.contextFrom",
		"task.policyActions",
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
		"stage.external-telemetry",
		"stage.run.command",
		"stage.run.script",
		"stage.run.env",
		"stage.run.syncBase",
		"stage.run.workspace.repo",
		"stage.run.workspace.scratch",
		"stage.workspace",
		"stage.workspace.repo-readonly",
		"gate.evaluator.agentic.workspace",
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
		"gate.evaluator.automated.check.failure-class",
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
		// The #3292 backfill: workflow/task/trigger/gate/goober surface that
		// shipped before the registry covered it.
		"workflow.spec.docsRoots",
		"workflow.spec.outboxMirrorPath",
		"workflow.spec.requires.capabilities",
		"workflow.spec.tutorScope",
		"workflow.spec.tutorScope.tier",
		"workflow.spec.tutorScope.target",
		"workflow.spec.runControls.maxRepasses",
		"workflow.spec.runControls.stalledRunTimeout",
		"workflow.spec.runControls.maxRunDuration",
		"task.outbox",
		"task.outboxMirrorPath",
		"trigger.priority",
		"trigger.idleBackoff",
		"trigger.idleBackoff.enabled",
		"trigger.idleBackoff.floor",
		"trigger.idleBackoff.ceiling",
		"gate.maxRepasses",
		"goober.spec.policyActions",
		"goober.spec.conditionalPolicyActions",
		// The DSL 3.0 surface (dsl-3.0.md §2/§4, issue #3505).
		"task.runsOn",
		"task.runsOn.os",
		"task.runsOn.cpu",
		"task.runsOn.memory",
		"task.runsOn.disk",
		"task.runsOn.capabilities",
		"task.runsOn.restrictions",
		"task.repoFrom",
		"task.commitsRepo",
	}
	slices.Sort(ids)
	return ids
}

// gaggleOnlyFeatureIDs are DSL features declared on Gaggle objects.
func gaggleOnlyFeatureIDs() []FeatureID {
	return []FeatureID{
		featureGaggleCheckoutSparse,
		// The #3292 backfill: the authorable Gaggle surface predated the
		// registry entirely, except sandbox and project.checkout.sparse.
		featureGaggleDisplayName,
		featureGaggleSelfIdentity,
		featureGaggleProject,
		featureGaggleProjectProviderGitHub,
		featureGaggleProjectProviderADO,
		featureGaggleProjectProviderGitea,
		featureGaggleProjectBaseURL,
		featureGaggleBacklog,
		featureGaggleBacklogProviderGitHub,
		featureGaggleBacklogProviderADO,
		featureGaggleBacklogProviderGitea,
		featureGaggleBacklogBaseURL,
		featureGaggleBacklogLabels,
		featureGaggleBacklogLabelPredicate,
		featureGaggleBacklogFieldPredicate,
		featureGaggleIsolationNamespace,
		featureGaggleIsolationIdentityRef,
		featureGaggleAdditionalRepos,
		featureGaggleAdditionalReposProviderGitHub,
		featureGaggleAdditionalReposProviderADO,
		featureGaggleAdditionalReposProviderGitea,
		featureGaggleAdditionalReposBaseURL,
		featureGaggleAdditionalReposCheckoutSparse,
		featureGaggleCICommand,
		featureGaggleBranchNamespace,
		featureGaggleRunControls,
		featureGaggleRunControlsMaxRepasses,
		featureGaggleRunControlsStalledRunTimeout,
		featureGaggleRunControlsMaxRunDuration,
		featureGaggleOutboxMirrorPath,
		featureGaggleWorkcopiesRoot,
		featureGaggleRequireLabels,
		featureGaggleSiblings,
		featureGaggleRunsOn,
		featureGaggleRunsOnOS,
		featureGaggleRunsOnCapabilities,
		featureGaggleRunsOnRestrictions,
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
