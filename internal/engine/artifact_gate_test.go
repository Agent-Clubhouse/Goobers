package engine

import (
	"context"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

type artifactGateCapture struct{ envelope apiv1.InvocationEnvelope }

func (c *artifactGateCapture) Evaluate(_ context.Context, _ apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	c.envelope = env
	return "pass", nil
}

func TestAutomatedGateProjectsAllUpstreamArtifactPointers(t *testing.T) {
	first := apiv1.ArtifactPointer{Path: "artifacts/first", Digest: apiv1.Digest([]byte("first")), Size: 5, MediaType: "text/plain", Integrity: apiv1.IntegrityDerived}
	second := apiv1.ArtifactPointer{Path: "artifacts/second", Digest: apiv1.Digest([]byte("second")), Size: 6, MediaType: "text/plain", Integrity: apiv1.IntegrityDerived}
	exec := newScriptedExec(map[string][]scriptedCall{
		"first":  {{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: []apiv1.ArtifactPointer{first}}}},
		"second": {{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: []apiv1.ArtifactPointer{second}}}},
	})
	capture := &artifactGateCapture{}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Det: exec, Auto: capture, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, RunInput{RunID: "artifact-gate", Gaggle: "web", WorkflowName: "artifacts", Version: 1, PreviewFeaturesEnabled: boolPointer(true), RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}, Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Start: "first", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Tasks: []apiv1.Task{detTask("first", "second"), detTask("second", "check")}, Gates: []apiv1.Gate{statusGate("check", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})},
	}})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	got := capture.envelope
	if got.Workspace != "" || len(got.Capabilities) != 0 {
		t.Fatalf("automated gate gained workspace/capabilities: %+v", got)
	}
	if len(got.ContextPointers) != 2 {
		t.Fatalf("context count: %+v", got.ContextPointers)
	}
	for i, want := range []apiv1.ArtifactPointer{first, second} {
		pointer := got.ContextPointers[i]
		if pointer.Name != []string{"first.artifact[0]", "second.artifact[0]"}[i] || pointer.Artifact == nil || *pointer.Artifact != want {
			t.Fatalf("slot %d: %+v, want %+v", i, pointer, want)
		}
	}
}
