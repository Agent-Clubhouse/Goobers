package gate

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func (e *Evaluator) reviewerContinuation(name string) (int, error) {
	if e.ReviewerContinuation == nil {
		return 0, nil
	}
	return e.ReviewerContinuation(name)
}

func recordReviewerStart(j Journal, gate apiv1.Gate, number int, class journal.AttemptClass) error {
	if j == nil {
		return nil
	}
	event := journal.ReviewerAttemptEvent(journal.EventReviewerStarted, gate.Name, number, class)
	if gate.Agentic != nil {
		event.Runner = map[string]any{"goober": gate.Agentic.Goober}
	}
	return j.Append(event)
}

func recordReviewerFinish(j Journal, name string, number int, class journal.AttemptClass, verdict apiv1.Verdict, err error, invalid bool) error {
	if j == nil {
		return nil
	}
	event := journal.ReviewerAttemptEvent(journal.EventReviewerFinished, name, number, class)
	event.Status = string(apiv1.ResultSuccess)
	event.Verdict = string(verdict.Decision)
	event.Runner = map[string]any{"reviewed": err == nil}
	if err != nil || invalid {
		event.Status = string(apiv1.ResultFailure)
		failureClass := journal.AttemptPolicy
		if invoke.IsInfrastructureFailure(err) {
			failureClass = journal.AttemptInfra
		}
		event.Runner["retryFailureClass"] = string(failureClass)
		if err != nil {
			event.Error = journal.ErrorDetailFor("evaluator_error", err)
		}
		if invalid {
			event.Error = &journal.ErrorDetail{Code: "verdict_invalid", Message: needsHumanRationaleFeedback}
		}
	}
	return j.Append(event)
}

type reviewerAttemptClassKey struct{}

// ReviewerAttemptClass returns the retry driver's class for the current
// reviewer dispatch. Heartbeats use this alongside the envelope's number.
func ReviewerAttemptClass(ctx context.Context) (journal.AttemptClass, bool) {
	class, ok := ctx.Value(reviewerAttemptClassKey{}).(journal.AttemptClass)
	return class, ok
}
