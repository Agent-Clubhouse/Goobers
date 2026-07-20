package v1alpha1

import (
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// roundTripStable marshals v to YAML, unmarshals into a fresh value of the same
// type, re-marshals, and asserts both the bytes and the decoded value are
// stable. This is the acceptance check: YAML -> struct -> YAML is stable.
func roundTripStable[T any](t *testing.T, v T) {
	t.Helper()
	yamlA, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded T
	if err := yaml.Unmarshal(yamlA, &decoded); err != nil {
		t.Fatalf("unmarshal: %v\n--- yaml ---\n%s", err, yamlA)
	}
	yamlB, err := yaml.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(yamlA) != string(yamlB) {
		t.Errorf("YAML not stable across round-trip:\n--- A ---\n%s\n--- B ---\n%s", yamlA, yamlB)
	}
	if !reflect.DeepEqual(v, decoded) {
		t.Errorf("decoded value differs from original:\n original: %#v\n decoded:  %#v", v, decoded)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := Manifest{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "Manifest"},
		ObjectMeta: metav1.ObjectMeta{Name: "acme-instance"},
		Spec: ManifestSpec{
			Instance: InstanceRef{Name: "acme", Environment: EnvironmentDev},
			Connections: []Connection{{
				Name:      "github-main",
				Type:      "repo",
				Provider:  "github",
				SecretRef: SecretRef{Name: "github-pat", KeyVault: "acme-kv"},
			}},
			Gaggles: []string{"acme-web"},
		},
	}
	roundTripStable(t, m)
}

func TestGaggleRoundTrip(t *testing.T) {
	g := Gaggle{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "Gaggle"},
		ObjectMeta: metav1.ObjectMeta{Name: "acme-web"},
		Spec: GaggleSpec{
			DisplayName: "Acme Web",
			Project:     RepoRef{Provider: ProviderGitHub, Owner: "acme", Name: "web", Branch: "main", ConnectionRef: "github-main"},
			Backlog:     BacklogRef{Provider: ProviderGitHub, Project: "acme/web", Labels: []string{"goobers"}, ConnectionRef: "github-backlog"},
			Isolation:   GaggleIsolation{Namespace: "gaggle-acme-web", IdentityRef: "acme-web-identity"},
		},
	}
	roundTripStable(t, g)
}

func TestGooberRoundTrip(t *testing.T) {
	g := Goober{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "Goober"},
		ObjectMeta: metav1.ObjectMeta{Name: "coder"},
		Spec: GooberSpec{
			Gaggle:       "acme-web",
			Role:         "coder",
			DisplayName:  "Coder",
			Instructions: "instructions.md",
			Harness:      HarnessCopilot,
			Model:        "claude-sonnet-5",
			HarnessOptions: map[string]apiextensionsv1.JSON{
				"context": {Raw: []byte(`"long_context"`)},
				"custom":  {Raw: []byte(`{"enabled":true,"limit":3}`)},
			},
			Capabilities: []string{"repo:push", "github:pr:write"},
			Skills:       []string{"implement", "run-tests"},
			Tools:        []string{"github", "shell"},
			ScaleFactor:  1,
			Workflows:    []string{"default-implement"},
		},
	}
	roundTripStable(t, g)
}

func TestWorkflowRoundTrip(t *testing.T) {
	w := Workflow{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "Workflow"},
		ObjectMeta: metav1.ObjectMeta{Name: "review-flow"},
		Spec: WorkflowSpec{
			Gaggle:      "acme-web",
			DisplayName: "Implement then review",
			Triggers: []Trigger{{
				Type:     TriggerBacklogItem,
				Selector: map[string]string{"goobers": "true"},
			}},
			Readiness: ReadinessConditions{MaxConcurrentRuns: 2, MaxRunsPerHour: 10, MaxChainDepth: 3},
			Start:     "implement",
			Tasks: []Task{
				{
					Name: "implement", Type: TaskAgentic, Goober: "coder",
					Goal: "Implement the item.", Capabilities: []string{"repo:push", "github:pr:write"},
					Retry:           &RetryPolicy{MaxAttempts: 2, BackoffSeconds: 30},
					TimeoutSeconds:  1800,
					Limits:          &Limits{MaxTokens: 2_000_000, MaxCostUSD: 5},
					ExpectedOutputs: []string{"pull-request"}, Next: "tests",
				},
				{
					Name: "tests", Type: TaskDeterministic,
					Run:  &DeterministicRun{Command: []string{"make", "test"}, Env: map[string]string{"CI": "true"}},
					Goal: "Run the test suite.", Next: "ci-gate",
				},
			},
			Gates: []Gate{
				{
					Name:      "ci-gate",
					Evaluator: EvaluatorAutomated,
					Automated: &AutomatedGate{
						Check:               "ci-status",
						TimeoutSeconds:      600,
						Retry:               &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 10},
						PollIntervalSeconds: 15,
					},
					Branches: map[string]string{"pass": "review", "fail": "review", "timeout": "review"},
				},
				{
					Name:      "review",
					Evaluator: EvaluatorAgentic,
					Agentic:   &AgenticGate{Goober: "reviewer", TimeoutSeconds: 900, Retry: &RetryPolicy{MaxAttempts: 2}},
					Branches:  map[string]string{"pass": "approve", "needs-changes": "implement"},
				},
				{
					Name:      "approve",
					Evaluator: EvaluatorHuman,
					Human:     &HumanGate{Approvers: []string{"group:leads"}, TimeoutSeconds: 86400, OnTimeout: "escalate"},
					Branches:  map[string]string{"pass": ""},
				},
			},
		},
	}
	roundTripStable(t, w)
}
