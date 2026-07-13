package gate

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type fakeGoober struct {
	invokeResult  apiv1.ResultEnvelope
	invokeErr     error
	reviewVerdict apiv1.Verdict
	reviewErr     error

	lastReviewEnv apiv1.InvocationEnvelope
	reviewCalls   int
}

func (f *fakeGoober) Invoke(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return f.invokeResult, f.invokeErr
}

func (f *fakeGoober) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	f.reviewCalls++
	f.lastReviewEnv = env
	return f.reviewVerdict, f.reviewErr
}

func TestReviewerInvocationAttachesEvidence(t *testing.T) {
	base := apiv1.InvocationEnvelope{
		Goal:            "review gate: code-review",
		ContextPointers: []apiv1.ContextPointer{{Name: "base-ptr", External: &apiv1.ExternalRef{Kind: "url", URI: "https://example.com"}}},
	}
	subject := apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess,
		Artifacts: []apiv1.ArtifactPointer{
			{Path: "artifacts/sha256/ab/cd", Digest: "sha256:" + fixedHex(), MediaType: "text/x-patch"},
		},
	}

	env := ReviewerInvocation(base, "implement", subject)

	if len(env.ContextPointers) != 2 {
		t.Fatalf("ContextPointers = %d, want 2 (base + evidence)", len(env.ContextPointers))
	}
	if env.ContextPointers[0].Name != "base-ptr" {
		t.Fatalf("base pointer not preserved: %+v", env.ContextPointers[0])
	}
	evidence := env.ContextPointers[1]
	if evidence.Name != "implement.artifact[0]" {
		t.Fatalf("evidence pointer name = %q, want %q", evidence.Name, "implement.artifact[0]")
	}
	if evidence.Artifact == nil || evidence.Artifact.Path != subject.Artifacts[0].Path {
		t.Fatalf("evidence pointer artifact = %+v, want %+v", evidence.Artifact, subject.Artifacts[0])
	}
	// Original base slice must not be mutated.
	if len(base.ContextPointers) != 1 {
		t.Fatalf("base.ContextPointers mutated: %+v", base.ContextPointers)
	}
}

func TestReviewerInvocationNoArtifactsIsNoop(t *testing.T) {
	base := apiv1.InvocationEnvelope{Goal: "review"}
	env := ReviewerInvocation(base, "implement", apiv1.ResultEnvelope{})
	if len(env.ContextPointers) != 0 {
		t.Fatalf("ContextPointers = %+v, want none", env.ContextPointers)
	}
}

func TestReviewerEvaluatorReview(t *testing.T) {
	fg := &fakeGoober{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "fix the thing"}}
	re := &ReviewerEvaluator{Goober: fg}

	base := apiv1.InvocationEnvelope{Goal: "review gate: code-review"}
	subject := apiv1.ResultEnvelope{Artifacts: []apiv1.ArtifactPointer{{Path: "p", Digest: "sha256:" + fixedHex()}}}

	got, err := re.Review(context.Background(), base, "implement", subject)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Decision != apiv1.VerdictNeedsChanges || got.Summary != "fix the thing" {
		t.Fatalf("Review result = %+v, want the fake's verdict", got)
	}
	if fg.reviewCalls != 1 {
		t.Fatalf("reviewCalls = %d, want 1", fg.reviewCalls)
	}
	if len(fg.lastReviewEnv.ContextPointers) != 1 {
		t.Fatalf("evidence not attached to the env passed to Goober.Review: %+v", fg.lastReviewEnv)
	}
}

func TestReviewerEvaluatorRequiresGoober(t *testing.T) {
	re := &ReviewerEvaluator{}
	if _, err := re.Review(context.Background(), apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}); err == nil {
		t.Fatal("want error when Goober is nil")
	}
}

func TestReviewerEvaluatorPropagatesError(t *testing.T) {
	fg := &fakeGoober{reviewErr: errors.New("boom")}
	re := &ReviewerEvaluator{Goober: fg}
	if _, err := re.Review(context.Background(), apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}); err == nil {
		t.Fatal("want error propagated from Goober.Review")
	}
}

func fixedHex() string {
	return "0000000000000000000000000000000000000000000000000000000000000000"[:64]
}
