package gate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/workflow"
)

func TestIssueStalenessGateAbortsForStaleIssue(t *testing.T) {
	paths := []string{
		filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "merge-review.yaml"),
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web", "workflows", "merge-review.yaml"),
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web-claude", "workflows", "merge-review.yaml"),
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal workflow: %v", err)
			}
			var stalenessGate apiv1.Gate
			for _, candidate := range w.Spec.Gates {
				if candidate.Name == "issue-staleness-gate" {
					stalenessGate = candidate
					break
				}
			}
			if stalenessGate.Name == "" {
				t.Fatal("issue-staleness-gate not found")
			}

			result, err := (&gate.Evaluator{
				Automated: gate.NewAutomatedEvaluator(),
			}).Evaluate(
				context.Background(),
				stalenessGate,
				apiv1.InvocationEnvelope{Inputs: map[string]interface{}{"issueStale": "true"}},
				"check-issue-staleness",
				apiv1.ResultEnvelope{},
				"",
				false,
			)
			if err != nil {
				t.Fatalf("evaluate issue-staleness-gate with stale issue: %v", err)
			}
			if result.Outcome != gate.OutcomeFail {
				t.Fatalf("stale issue gate outcome = %q, want fail", result.Outcome)
			}
			target, ok := workflow.BranchTarget(stalenessGate, result.Outcome)
			if !ok || target != workflow.TargetAbort {
				t.Fatalf("stale issue gate target = %q,%v, want %q,true", target, ok, workflow.TargetAbort)
			}
			if result.Target != workflow.TargetAbort {
				t.Fatalf("stale issue gate result target = %q, want %q", result.Target, workflow.TargetAbort)
			}
		})
	}
}
