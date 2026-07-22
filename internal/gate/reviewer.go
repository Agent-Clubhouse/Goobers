package gate

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
)

// ReviewerInvocation returns base with the subject stage's artifacts attached
// as evidence ContextPointers (issue #20: "reviewer instructions + evidence
// pointers"). base already carries the reviewer instructions as its Goal
// (built by the caller — the runner — same as any other invocation); this
// only adds the pointers a reviewer needs to actually inspect the work.
func ReviewerInvocation(base apiv1.InvocationEnvelope, subjectStage string, subject apiv1.ResultEnvelope) apiv1.InvocationEnvelope {
	env := base
	if len(subject.Artifacts) == 0 {
		return env
	}
	env.ContextPointers = append(append([]apiv1.ContextPointer{}, base.ContextPointers...), evidencePointers(subjectStage, subject)...)
	return env
}

func evidencePointers(subjectStage string, subject apiv1.ResultEnvelope) []apiv1.ContextPointer {
	ptrs := make([]apiv1.ContextPointer, 0, len(subject.Artifacts))
	for i, a := range subject.Artifacts {
		a := a
		ptrs = append(ptrs, apiv1.ContextPointer{
			Name:     fmt.Sprintf("%s.artifact[%d]", subjectStage, i),
			Artifact: &a,
		})
	}
	return ptrs
}

// ReviewerEvaluator is the agentic gate evaluator: it attaches evidence
// pointers to the reviewer's invocation and invokes the reviewer goober
// (invoke.Goober.Review, issue #19's seam) to get back a Verdict. It never
// interprets the Verdict itself — mapping Decision to a branch outcome is
// Evaluator's job (evaluate.go), which also owns bounded repass + journaling.
type ReviewerEvaluator struct {
	Goober invoke.Goober
}

// Review builds the reviewer's evidence-attached invocation and invokes it.
//
// A reviewer session that produced no verdict file (harness.ErrNoCompletion —
// the copilot session hit a limit/timeout mid-work and never wrote
// .goobers/verdict.json, #765) is a TRANSIENT harness failure, not a review
// outcome. It is marked as an infrastructure failure so the gate evaluator
// retries it within the gate's declared RetryPolicy bound instead of failing
// the run on the first occurrence. This classification lives here, at the gate's
// reviewer seam, so only agentic GATE evaluation gains the retry — agentic
// stages, which never go through ReviewerEvaluator, are unaffected.
func (e *ReviewerEvaluator) Review(ctx context.Context, base apiv1.InvocationEnvelope, subjectStage string, subject apiv1.ResultEnvelope) (apiv1.Verdict, error) {
	if e.Goober == nil {
		return apiv1.Verdict{}, fmt.Errorf("gate: reviewer evaluator has no Goober configured")
	}
	v, err := e.Goober.Review(ctx, ReviewerInvocation(base, subjectStage, subject))
	if err != nil && errors.Is(err, harness.ErrNoCompletion) {
		return v, invoke.InfrastructureFailure(err)
	}
	return v, err
}
