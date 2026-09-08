package runner

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
)

func TestAutomatedGateReceivesUpstreamArtifactsWithoutWorkspace(t *testing.T) {
	const runID = "artifact-gate"
	auto := &envelopeCapturingAutomated{}
	r, _ := newTestRunner(t, map[string]stubTaskResult{runID + ":implement": {
		status: apiv1.ResultSuccess, artifactName: "evidence", artifactData: []byte("evidence"), artifactMediaType: "text/plain",
	}}, auto)
	result, err := r.Start(context.Background(), StartInput{RunID: runID, Machine: fixtureMachine(t), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase: %s", result.Phase)
	}
	if auto.env.Workspace != "" || len(auto.env.Capabilities) != 0 {
		t.Fatalf("automated gate received workspace or capabilities: %+v", auto.env)
	}
	pointers := auto.env.ContextPointers
	if len(pointers) != 1 || pointers[0].Name != "implement.artifact[0]" || pointers[0].Artifact == nil || pointers[0].Artifact.Digest != apiv1.Digest([]byte("evidence")) {
		t.Fatalf("missing upstream artifact: %+v", pointers)
	}
}

func TestArtifactAwareGateReadsItsRunJournal(t *testing.T) {
	const runID = "artifact-reader-gate"
	called := false
	auto := &gate.AutomatedEvaluator{Checks: map[string]gate.CheckFunc{}, ArtifactChecks: map[string]gate.ArtifactCheckFunc{
		"status-equals": func(ctx context.Context, _ map[string]interface{}, _ map[string]string, pointers []apiv1.ContextPointer, reader artifactset.Reader) (string, error) {
			called = true
			if len(pointers) != 1 || pointers[0].Artifact == nil {
				t.Fatalf("missing evidence: %+v", pointers)
			}
			data, err := reader.ReadArtifact(ctx, *pointers[0].Artifact, 100)
			if err != nil {
				return "", err
			}
			if string(data) != "actual journal evidence" {
				t.Fatalf("unexpected evidence: %q", data)
			}
			return gate.OutcomePass, nil
		},
	}}
	r, _ := newTestRunner(t, map[string]stubTaskResult{runID + ":implement": {status: apiv1.ResultSuccess, artifactName: "evidence", artifactData: []byte("actual journal evidence"), artifactMediaType: "text/plain"}}, auto)
	result, err := r.Start(context.Background(), StartInput{RunID: runID, Machine: fixtureMachine(t), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if !called || result.Phase != journal.PhaseCompleted {
		t.Fatalf("check not completed: called=%v phase=%s", called, result.Phase)
	}
	if auto.OpenArtifacts != nil {
		t.Fatal("runner mutated shared evaluator")
	}
}
