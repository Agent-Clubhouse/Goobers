package engine

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
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
func dispatchWithRetry(ctx workflow.Context, t apiv1.Task, rec *runJournal, pointers []apiv1.ContextPointer, dispatch func(workflow.Context) (stageActivityResult, error)) (apiv1.ResultEnvelope, error) {
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

		// Projection parity with runTask's attempt journaling: stage.started
		// and the context manifest are committed before the executor runs
		// (the local runner's own order — both stamped with the pre-dispatch
		// time), then emitted live (DS4) so the attempt is visible before it
		// executes, not minutes after it closes. Lazy run-branch provenance
		// and the attempt's own outcome event follow the dispatch.
		mark := rec.mark()
		startedAt := workflow.Now(ctx)
		rec.stageStarted(startedAt, t.Name, int(attempt), class)
		if merr := rec.contextManifest(startedAt, t.Name, int(attempt), class, pointers); merr != nil {
			return apiv1.ResultEnvelope{}, merr
		}
		var res apiv1.ResultEnvelope
		var err error
		emitErr := rec.emitPending(ctx)
		if emitErr == nil {
			var activityResult stageActivityResult
			activityResult, err = dispatch(ctx)
			res = activityResult.ResultEnvelope
			if temporal.IsCanceledError(err) || ctx.Err() != nil {
				return apiv1.ResultEnvelope{}, err
			}
			rec.recordDeferredRunBranch(ctx, err, res, len(activityResult.Mutations) > 0)
			if err == nil {
				res.Artifacts = normalizeArtifactIntegrity(t.Type, res.Artifacts)
				rec.mutationIssues(ctx, t.Name, int(attempt), class, activityResult.MutationIssues)
				rec.mutations(ctx, t.Name, int(attempt), class, activityResult.Mutations)
				rec.stageFinished(ctx, t.Name, int(attempt), class, res, t.ContinueOnError)
				emitErr = rec.emitPending(ctx)
				if emitErr == nil {
					return res, nil
				}
			}
		}
		if emitErr != nil {
			// §8 failure policy: a journal emission that exhausted its bounded
			// budget fails the attempt as attemptClass infra — never the work
			// budget (#3361). The attempt's un-journaled ops are rolled back
			// first (an effect that cannot be journaled did not happen; the
			// same fail-closed stance the projection takes), so the projection
			// and the live journal agree the attempt produced only its
			// infra-classed failure record.
			rec.rollbackUnemitted(mark)
			rec.emitFailure(ctx, t.Name, int(attempt), emitErr)
			lastErr = emitErr
			infrastructureFailures++
			nextRetryClass = journal.AttemptInfra
			if infrastructureFailures >= runner.DefaultMaxInfrastructureAttempts {
				return apiv1.ResultEnvelope{}, fmt.Errorf(
					"engine: journal stage %q: %w (attempt %d/%d)",
					t.Name, lastErr, infrastructureFailures, runner.DefaultMaxInfrastructureAttempts)
			}
			if retryDelay := infrastructureRetryDelay(emitErr, backoff, workflow.Now(ctx)); retryDelay > 0 {
				if serr := workflow.Sleep(ctx, retryDelay); serr != nil {
					return apiv1.ResultEnvelope{}, serr
				}
			}
			continue
		}
		lastErr = err
		failureClass, cerr := attemptFailureClass(err)
		if cerr != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute stage %q: %w", t.Name, cerr)
		}
		rec.executorError(ctx, t.Name, int(attempt), class, failureClass, err)
		if isStartToCloseTimeout(err) && len(t.PolicyActions) > 0 {
			return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute side-effecting stage %q: refusing to retry after worker loss: %w", t.Name, err)
		}
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
		retryDelay := infrastructureRetryDelay(err, backoff, workflow.Now(ctx))
		if retryDelay > 0 {
			if serr := workflow.Sleep(ctx, retryDelay); serr != nil {
				return apiv1.ResultEnvelope{}, serr
			}
		}
	}
	// Unreachable: maxAttempts >= 1 always executes the loop body at least
	// once, and every path inside either returns or continues.
	return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute stage %q: exhausted attempts: %w", t.Name, lastErr)
}

func infrastructureRetryDelay(err error, backoff time.Duration, now time.Time) time.Duration {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != FailureTypeInfrastructure || !appErr.HasDetails() {
		return backoff
	}
	var retryAt time.Time
	if err := appErr.Details(&retryAt); err != nil {
		return backoff
	}
	until := retryAt.Sub(now)
	if until > backoff {
		return until
	}
	return backoff
}

// attemptFailureClass maps one failed dispatch to the journal attempt class
// its retry would consume, derived purely from the error shape Temporal
// records in history — no side-channel state, so the projection (#629) can
// re-derive the identical classes:
//
//   - an application error typed FailureTypeInfrastructure is infrastructure;
//   - any other application error is policy (the local runner's
//     dispatchRetryFailureClass rule: everything unmarked is policy-driven).
//     A stage overrunning its declared duration limit lands here: the worker
//     self-enforces the limit and surfaces it as invoke.Timeout →
//     FailureTypeStage, the same policy class the local runner assigns (#724);
//   - a Temporal ScheduleToStart timeout is infrastructure because the
//     activity never began. StartToClose is policy-classed because the worker
//     may have been lost after the stage committed an external effect; a task
//     declaring policyActions is therefore stopped before retry;
//   - anything else fails closed as unclassifiable. A projection error, never
//     a silent default to "infra".
func attemptFailureClass(err error) (journal.AttemptClass, error) {
	var timeoutErr *temporal.TimeoutError
	if errors.As(err, &timeoutErr) {
		switch timeoutErr.TimeoutType() {
		case enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START:
			return journal.AttemptInfra, nil
		case enumspb.TIMEOUT_TYPE_START_TO_CLOSE:
			return journal.AttemptPolicy, nil
		default:
			return "", fmt.Errorf("unclassifiable Temporal timeout type %q (refusing a silent %q default): %w", timeoutErr.TimeoutType(), journal.AttemptInfra, err)
		}
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		if appErr.Type() == FailureTypeInfrastructure {
			return journal.AttemptInfra, nil
		}
		return journal.AttemptPolicy, nil
	}
	return "", fmt.Errorf("unclassifiable attempt failure (refusing a silent %q default): %w", journal.AttemptInfra, err)
}

func isStartToCloseTimeout(err error) bool {
	var timeoutErr *temporal.TimeoutError
	return errors.As(err, &timeoutErr) && timeoutErr.TimeoutType() == enumspb.TIMEOUT_TYPE_START_TO_CLOSE
}
