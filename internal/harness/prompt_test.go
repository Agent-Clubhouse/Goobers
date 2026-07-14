package harness

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestRenderPromptReferencesResolvedContextPath is the regression test for
// #121: an Artifact-backed ContextPointer resolved into the workspace (see
// materializeContext) must be rendered as an actionable local path, not the
// bare, opaque pointer name a harness has no way to act on.
func TestRenderPromptReferencesResolvedContextPath(t *testing.T) {
	req := RunRequest{
		Envelope: apiv1.InvocationEnvelope{
			Goal: "review the change",
			ContextPointers: []apiv1.ContextPointer{
				{Name: "implement.artifact[0]", Artifact: &apiv1.ArtifactPointer{Path: "artifacts/x", Digest: apiv1.Digest([]byte("x"))}},
			},
		},
		ContextPaths:   map[string]string{"implement.artifact[0]": ".goobers/context/00-implement.artifact_0_"},
		CompletionPath: DefaultResultPath,
	}
	prompt := renderPrompt(req)
	if !strings.Contains(prompt, ".goobers/context/00-implement.artifact_0_") {
		t.Fatalf("prompt missing resolved context path: %q", prompt)
	}
	if !strings.Contains(prompt, "implement.artifact[0]") {
		t.Fatalf("prompt missing context pointer name: %q", prompt)
	}
}

// TestRenderPromptFallsBackToBareNameWithoutResolvedPath covers an External
// pointer (nothing to resolve — materializeContext only ever populates
// ContextPaths for Artifact-backed pointers) and confirms rendering is
// unchanged from before #121: just the bare name, no fabricated path.
func TestRenderPromptFallsBackToBareNameWithoutResolvedPath(t *testing.T) {
	req := RunRequest{
		Envelope: apiv1.InvocationEnvelope{
			Goal: "review the change",
			ContextPointers: []apiv1.ContextPointer{
				{Name: "issue-42", External: &apiv1.ExternalRef{Kind: "issue", URI: "https://example/issues/42"}},
			},
		},
		CompletionPath: DefaultResultPath,
	}
	prompt := renderPrompt(req)
	if !strings.Contains(prompt, "issue-42") {
		t.Fatalf("prompt missing context pointer name: %q", prompt)
	}
	if strings.Contains(prompt, "available at") {
		t.Fatalf("prompt fabricated a resolved path for an unresolved pointer: %q", prompt)
	}
}
