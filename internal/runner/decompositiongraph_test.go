package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func loadShippedDecomposition(t *testing.T) *workflow.Machine {
	t.Helper()
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")
	raw, err := os.ReadFile(filepath.Join(root, "workflows", "decomposition.yaml"))
	if err != nil {
		t.Fatalf("read decomposition workflow: %v", err)
	}
	var definition apiv1.Workflow
	if err := yaml.Unmarshal(raw, &definition); err != nil {
		t.Fatalf("unmarshal decomposition workflow: %v", err)
	}
	raw, err = os.ReadFile(filepath.Join(root, "goobers", "decomposer", "goober.yaml"))
	if err != nil {
		t.Fatalf("read decomposer goober: %v", err)
	}
	var decomposer apiv1.Goober
	if err := yaml.Unmarshal(raw, &decomposer); err != nil {
		t.Fatalf("unmarshal decomposer goober: %v", err)
	}
	machine, err := workflow.Compile(
		workflow.Definition{Name: definition.Name, Version: 1, Spec: definition.Spec},
		workflow.WithGoobers(map[string]apiv1.GooberSpec{decomposer.Name: decomposer.Spec}),
		workflow.WithKnownChecks([]string{"output-equals"}),
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile decomposition workflow: %v", err)
	}
	return machine
}

func TestShippedDecompositionThreadsConflictReasonToCloseOut(t *testing.T) {
	tests := []struct {
		name             string
		validateOutputs  map[string]interface{}
		publishOutputs   map[string]interface{}
		wantReason       string
		wantPublishStage bool
	}{
		{
			name: "validation conflict",
			validateOutputs: map[string]interface{}{
				"valid": false, "planDigest": "", "errors": []string{}, "conflict": true,
				"conflictReason":     "parent changed from digest old to digest new",
				"unresolvedDecision": false, "unresolvedDecisionReason": "", "schemaInvalid": false, "repassable": false,
			},
			wantReason: "parent changed from digest old to digest new",
		},
		{
			name: "publication conflict",
			validateOutputs: map[string]interface{}{
				"valid": true, "planDigest": "sha256:plan", "errors": []string{}, "conflict": false,
				"conflictReason": "", "unresolvedDecision": false, "unresolvedDecisionReason": "", "schemaInvalid": false,
				"repassable": false,
			},
			publishOutputs: map[string]interface{}{
				"parentId": "2247", "planDigest": "sha256:plan", "childIds": []string{},
				"publicationConflict": true, "conflictReason": "parent revision changed while publishing",
			},
			wantReason:       "parent revision changed while publishing",
			wantPublishStage: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const runID = "decomposition-conflict"
			byTask := map[string]stubTaskResult{
				runID + ":select-source": {
					status: apiv1.ResultSuccess,
					outputs: map[string]interface{}{
						"claimed": true, "noWork": false, "mode": "escalation", "sourceRunId": "source",
						"parent": "2247", "issueSnapshotDigest": "sha256:parent",
					},
				},
				runID + ":validate-plan":  {status: apiv1.ResultSuccess, outputs: tt.validateOutputs},
				runID + ":park-for-human": {status: apiv1.ResultSuccess},
			}
			if tt.wantPublishStage {
				byTask[runID+":publish-slices"] = stubTaskResult{status: apiv1.ResultSuccess, outputs: tt.publishOutputs}
			}
			deterministic := &outputCapturingDeterministic{byTask: byTask}
			runsDir, fixtureRepo, worktrees := newTestRunnerEnv(t)
			r, err := New(Config{
				NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
					deterministic.rec = rec
					return deterministic, nil
				},
				NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
					return &capturingSuccessGoober{}, nil
				},
				Automated:    gate.NewAutomatedEvaluator(),
				Worktrees:    worktrees,
				RunsDir:      runsDir,
				RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
			})
			if err != nil {
				t.Fatalf("new runner: %v", err)
			}
			result, err := r.Start(context.Background(), StartInput{
				RunID: runID, Machine: loadShippedDecomposition(t), Gaggle: "goobers",
				Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "widgets", Branch: "main"},
			})
			if err != nil {
				t.Fatalf("start decomposition: %v", err)
			}
			if result.Phase != journal.PhaseEscalated {
				t.Fatalf("phase = %q, want escalated after parking", result.Phase)
			}
			park, ok := deterministic.received[runID+":park-for-human"]
			if !ok {
				t.Fatal("park-for-human was not dispatched")
			}
			if got := park.Inputs["reason"]; got != tt.wantReason {
				t.Fatalf("park reason = %v, want %q", got, tt.wantReason)
			}
		})
	}
}

func TestShippedDecompositionSchemaInvalidAbortsBeforeRepass(t *testing.T) {
	const runID = "decomposition-schema-invalid"
	deterministic := &outputCapturingDeterministic{byTask: map[string]stubTaskResult{
		runID + ":select-source": {
			status: apiv1.ResultSuccess,
			outputs: map[string]interface{}{
				"claimed": true, "noWork": false, "mode": "escalation", "sourceRunId": "source",
				"parent": "2247", "issueSnapshotDigest": "sha256:parent",
			},
		},
		runID + ":validate-plan": {
			status: apiv1.ResultSuccess,
			outputs: map[string]interface{}{
				"valid": false, "planDigest": "", "errors": []string{"unsupported schema"}, "conflict": false,
				"conflictReason": "", "unresolvedDecision": false, "unresolvedDecisionReason": "", "schemaInvalid": true,
				"repassable": false,
			},
		},
	}}
	goober := &capturingSuccessGoober{}
	runsDir, fixtureRepo, worktrees := newTestRunnerEnv(t)
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			deterministic.rec = rec
			return deterministic, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    worktrees,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: loadShippedDecomposition(t), Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "widgets", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("start decomposition: %v", err)
	}
	if result.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want fail-closed abort", result.Phase)
	}
	if len(goober.invocations) != 1 {
		t.Fatalf("design-slices invocations = %d, want no schema-invalid repass", len(goober.invocations))
	}
	if _, parked := deterministic.received[runID+":park-for-human"]; parked {
		t.Fatal("schema-invalid plan was parked instead of aborted")
	}
}
