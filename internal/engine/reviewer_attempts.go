package engine

import (
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const reviewerAttemptLifecycleChange = "reviewer-attempt-lifecycle-v1"

type reviewerNumberKey struct{}

func reviewerNumber(ctx workflow.Context) (int, bool) {
	number, ok := ctx.Value(reviewerNumberKey{}).(int)
	return number, ok
}

// Keep both journal activity payloads and reviewer envelopes byte-compatible
// when replaying histories recorded before per-dispatch reviewer lifecycle.
func recordReviewerDispatch(ctx workflow.Context, rec *runJournal, g apiv1.Gate, number int, class journal.AttemptClass, review *GateReviewResult, call func(workflow.Context, journal.AttemptClass) error) error {
	if workflow.GetVersion(ctx, reviewerAttemptLifecycleChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return call(ctx, class)
	}
	// Each graph traversal starts a fresh visit, independent of the gate's
	// policy repass budget. Only retries within this traversal carry a class.
	if number == 1 {
		class = ""
	}
	event := journal.ReviewerAttemptEvent(journal.EventReviewerStarted, g.Name, number, class)
	if g.Agentic != nil {
		event.Runner = map[string]any{"goober": g.Agentic.Goober}
	}
	rec.append(ctx, event)
	if err := rec.emitPending(ctx); err != nil {
		return err
	}
	err := call(workflow.WithValue(ctx, reviewerNumberKey{}, number), class)
	event = journal.ReviewerAttemptEvent(journal.EventReviewerFinished, g.Name, number, class)
	event.Status = string(apiv1.ResultSuccess)
	if err == nil {
		event.Verdict = string(review.Decision)
		// Workspace-based shortcuts occur inside the activity. Preserve that
		// distinction without pretending this dispatch launched a model.
		event.Runner = map[string]any{"reviewed": review.Reviewed}
	} else {
		event.Status = string(apiv1.ResultFailure)
		event.Error = journal.ErrorDetailFor("evaluator_error", err)
		failureClass, _ := ClassifyDispatchFailure(err)
		event.Runner = map[string]any{"retryFailureClass": string(failureClass)}
	}
	rec.append(ctx, event)
	if emitErr := rec.emitPending(ctx); emitErr != nil {
		return emitErr
	}
	return err
}
