package harness

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
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

// TestResultShapeHintPresentsErrorConditionally is #297's regression guard for
// the fix: the result completion-contract hint must NOT present "error" as an
// always-present field of the JSON shape. A model that faithfully fills every
// shown field on a successful run would otherwise emit an empty error object,
// which fails the schema's errorInfo minLength:1 check and journals a correct
// run as a false-negative "failed". error must be described as conditional
// (failure/blocked only), and success told to omit it.
func TestResultShapeHintPresentsErrorConditionally(t *testing.T) {
	req := RunRequest{
		Envelope:       apiv1.InvocationEnvelope{Goal: "do the thing"},
		CompletionPath: DefaultResultPath,
		Mode:           ModeInvoke,
	}
	prompt := renderPrompt(req)
	// The always-present shape must not carry error inline (the #297 bug shape).
	if strings.Contains(prompt, `"metrics": {...}, "error"`) {
		t.Fatalf("result shape still presents error as an always-present field (#297): %q", prompt)
	}
	if !strings.Contains(prompt, `Omit "error" entirely on success`) {
		t.Fatalf("result hint missing the conditional-error note: %q", prompt)
	}
	if !strings.Contains(prompt, "failure") || !strings.Contains(prompt, "blocked") {
		t.Fatalf("result hint should scope error to failure/blocked: %q", prompt)
	}
}

// TestResultEnvelopeErrorContract pins the schema behavior the #297 prompt fix
// aligns the model output to (Lead's ruling: fix the template, NOT the schema).
// A success result omitting error validates; the #297 bug shape (success with an
// empty error object) is rejected; and a failure without error is still rejected
// — the load-bearing "a failure must carry error detail" contract stays intact.
func TestResultEnvelopeErrorContract(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatalf("validate.New: %v", err)
	}
	cases := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{"success without error validates", `{"status":"success","summary":"did it"}`, false},
		{"success with empty error rejected (the #297 false-negative)", `{"status":"success","error":{"code":"","message":""}}`, true},
		{"failure without error rejected", `{"status":"failure","summary":"nope"}`, true},
		{"failure with valid error validates", `{"status":"failure","error":{"code":"E_BOOM","message":"boom"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v.ValidateEnvelope("result", []byte(tc.json))
			if tc.wantErr && err == nil {
				t.Fatalf("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
