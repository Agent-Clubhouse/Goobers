package engine

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// Failure types stamped onto activity errors so an attempt's class survives
// into workflow history (#622). The invoke-level infrastructure marker cannot
// cross the activity boundary — the SDK serializes errors — so activities
// commit the class into the application-error type, and both this loop and
// the history→journal projection (#629) read it back from history alone.
const (
	// FailureTypeInfrastructure marks a seam error the runtime tagged
	// invoke.InfrastructureFailure: transient external infrastructure,
	// consuming the bounded infrastructure budget (journal class "infra",
	// excluded from conformance).
	FailureTypeInfrastructure = "GoobersInfrastructureFailure"
	// FailureTypeStage marks every other seam error — the policy class. Same
	// rule as the local runner's dispatchRetryFailureClass: everything
	// unmarked is policy-driven.
	FailureTypeStage = "GoobersStageFailure"
)

// dispatchWithRetry drives one task's attempt loop, mirroring
// internal/runner.runTask's budget arithmetic exactly: Task.Retry bounds
// policy-class attempts (defaulting to 1 — a task with no retry block gets
// exactly one policy attempt, never Temporal's unlimited default), and
// runner.DefaultMaxInfrastructureAttempts separately bounds infrastructure
// recoveries, the combined ceiling being policy+infra-1 dispatches. Retry
// orchestration deliberately lives here in the workflow rather than in a
// Temporal RetryPolicy: Temporal's single MaximumAttempts cannot express the
// split policy/infrastructure budgets, while this loop keeps every history
// attempt 1:1 with a journal attempt whose class is derivable from the prior
// attempt's recorded failure type (attemptFailureClass). Each dispatch still
// carries an explicit RetryPolicy{MaximumAttempts: 1} (stageActivityOptions)
// so the unlimited default is structurally unreachable.
func dispatchWithRetry(ctx workflow.Context, t apiv1.Task, rec *runJournal, pointers []apiv1.ContextPointer, dispatch func(workflow.Context) (apiv1.ResultEnvelope, error)) (apiv1.ResultEnvelope, error) {
	policyMaxAttempts := int32(1)
	var backoff time.Duration
	if t.Retry != nil {
		if t.Retry.MaxAttempts > 0 {
			policyMaxAttempts = t.Retry.MaxAttempts
		}
		backoff = time.Duration(t.Retry.BackoffSeconds) * time.Second
	}
	// The infrastructure budget includes its triggering failure, so it can add
	// at most MaxInfrastructureAttempts-1 dispatches to the policy budget.
	maxAttempts := policyMaxAttempts + runner.DefaultMaxInfrastructureAttempts - 1

	var policyAttempts, infrastructureFailures int32
	var lastErr error
	nextRetryClass := journal.AttemptPolicy
	for attempt := int32(1); attempt <= maxAttempts; attempt++ {
		// The first attempt carries no class (normative); a retry carries the
		// class its triggering failure selected — the journal attempt-class
		// convention (§3.3). Infrastructure retries do not consume the policy
		// budget.
		class := journal.AttemptClass("")
		if attempt > 1 {
			class = nextRetryClass
		}
		if class != journal.AttemptInfra {
			policyAttempts++
		}

		startedAt := workflow.Now(ctx)
		res, err := dispatch(ctx)
		if temporal.IsCanceledError(err) || ctx.Err() != nil {
			return apiv1.ResultEnvelope{}, err
		}
		// Projection parity with runTask's attempt journaling: stage.started,
		// the context manifest committed before the executor ran (both stamped
		// with the pre-dispatch time), lazy run-branch provenance, then the
		// attempt's own outcome event.
		rec.stageStarted(startedAt, t.Name, int(attempt), class)
		if merr := rec.contextManifest(startedAt, t.Name, int(attempt), class, pointers); merr != nil {
			return apiv1.ResultEnvelope{}, merr
		}
		rec.recordDeferredRunBranch(ctx, err, res)
		if err == nil {
			rec.stageFinished(ctx, t.Name, int(attempt), class, res, t.ContinueOnError)
			return res, nil
		}
		lastErr = err
		failureClass, cerr := attemptFailureClass(err)
		if cerr != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute stage %q: %w", t.Name, cerr)
		}
		rec.executorError(ctx, t.Name, int(attempt), class, failureClass, err)
		retryLimit, retryCount := policyMaxAttempts, policyAttempts
		shouldRetry := policyAttempts < policyMaxAttempts
		nextRetryClass = journal.AttemptPolicy
		if failureClass == journal.AttemptInfra {
			infrastructureFailures++
			retryLimit, retryCount = runner.DefaultMaxInfrastructureAttempts, infrastructureFailures
			shouldRetry = infrastructureFailures < runner.DefaultMaxInfrastructureAttempts
			nextRetryClass = journal.AttemptInfra
		}
		if !shouldRetry {
			return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute stage %q: %w (attempt %d/%d)", t.Name, lastErr, retryCount, retryLimit)
		}
		if backoff > 0 {
			if serr := workflow.Sleep(ctx, backoff); serr != nil {
				return apiv1.ResultEnvelope{}, serr
			}
		}
	}
	// Unreachable: maxAttempts >= 1 always executes the loop body at least
	// once, and every path inside either returns or continues.
	return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute stage %q: exhausted attempts: %w", t.Name, lastErr)
}

// attemptFailureClass maps one failed dispatch to the journal attempt class
// its retry would consume, derived purely from the error shape Temporal
// records in history — no side-channel state, so the projection (#629) can
// re-derive the identical classes:
//
//   - an application error typed FailureTypeInfrastructure is infrastructure;
//   - any other application error is policy (the local runner's
//     dispatchRetryFailureClass rule: everything unmarked is policy-driven);
//   - a Temporal timeout — start-to-close expiry, worker loss before the
//     stage produced a verdict — is infrastructure: the platform failed, not
//     the stage's policy (#622);
//   - anything else fails closed as unclassifiable. A projection error, never
//     a silent default to "infra".
func attemptFailureClass(err error) (journal.AttemptClass, error) {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		if appErr.Type() == FailureTypeInfrastructure {
			return journal.AttemptInfra, nil
		}
		return journal.AttemptPolicy, nil
	}
	var timeoutErr *temporal.TimeoutError
	if errors.As(err, &timeoutErr) {
		return journal.AttemptInfra, nil
	}
	return "", fmt.Errorf("unclassifiable attempt failure (refusing a silent %q default): %w", journal.AttemptInfra, err)
}
