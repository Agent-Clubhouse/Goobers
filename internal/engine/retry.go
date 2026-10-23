package engine

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/attemptidentity"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/usageallowance"
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
	// FailureTypeWorkspaceHeadMoved marks a reviewer pod that refused to land
	// the run's workspace delta because the pull request head the run rebound
	// to moved under it (#6019, runner.WorkspaceHeadMovedErrorCode). Policy
	// class to ClassifyDispatchFailure and non-retryable — a fresh pod clones
	// the same moved head and re-derives the same refusal (#5948) — and the
	// walk ends the run superseded on it instead of failing (IsWorkspaceHeadMoved).
	FailureTypeWorkspaceHeadMoved = "GoobersWorkspaceHeadMoved"
)

// IsWorkspaceHeadMoved reports whether err is a reviewer evaluation that
// ended on a pull request head moving under the run (#6019).
func IsWorkspaceHeadMoved(err error) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.Type() == FailureTypeWorkspaceHeadMoved
}

// reservedHeadMovedCode is what admitWorkspaceHeadMoved renames a
// workspace_head_moved code to when nothing vouches for it.
const reservedHeadMovedCode = "workspace_head_moved_unattested"

// admitWorkspaceHeadMoved enforces that only the pod runtime's own workspace
// provisioning can end a run superseded (#6019): a workspace_head_moved code
// the runtime did not attest (attested=false) is a stage's own claim — its
// result file, or an in-process executor's — and is renamed so it fails like
// any other stage failure instead of skipping gate routing and every streak.
func admitWorkspaceHeadMoved(result *apiv1.ResultEnvelope, attested bool) {
	if attested || result.Error == nil || result.Error.Code != runsemantics.WorkspaceHeadMovedErrorCode {
		return
	}
	result.Error.Code = reservedHeadMovedCode
	result.Error.Message = "stage reported the runtime-reserved code " + runsemantics.WorkspaceHeadMovedErrorCode + ": " + result.Error.Message
}

// workspaceHeadMovedResult reports whether a stage's surrendered failure is a
// pull request head that moved under the run (#6019). Only an attested code
// survives admitWorkspaceHeadMoved to reach here.
func workspaceHeadMovedResult(result apiv1.ResultEnvelope) bool {
	return result.Status == apiv1.ResultFailure && result.Error != nil && result.Error.Code == runsemantics.WorkspaceHeadMovedErrorCode
}

// gateEvaluationFailed is the walk's answer to a gate evaluation that
// returned an error: the walk fails with it — except when the gate's reviewer
// pod could not land the run's delta on a pull request head that moved under
// the run (#6019). Then there is no verdict to route on and nothing left to
// review, and the run ends superseded at the gate. A completed terminal is
// never HITL-resumable (hitlResumable), so the walk returns it directly rather
// than through its settle hook.
func gateEvaluationFailed(ctx workflow.Context, rec *runJournal, gate string, err error, upstream map[string]apiv1.ResultEnvelope, steps int) (RunResult, error) {
	if !IsWorkspaceHeadMoved(err) {
		return RunResult{}, err
	}
	rec.workspaceHeadMoved(ctx, gate, err.Error())
	return supersededRunResult(gate, upstream, steps), nil
}

// supersededRunResult is the terminal a run reaches when its subject moved
// under it (#6019): completed, never failed — nothing about the run's work
// failed, the pull request it was working on simply is not at the revision
// the work was built on any more. The claim is released by terminal
// finalization like any other run's, so the next cycle selects the PR at its
// new head, and the terminal handlers recognise the journaled
// workspace_head_moved marker and neither charge nor credit the item.
//
// NoWork is deliberately NOT set. It is the scheduler's first-stage
// "this tick found nothing" signal (taskOutcome's steps == 1, the local
// runner's identical rule) and drives idle backoff for the whole lane; a
// superseded run ran stages, often a full agent session, so it ends
// `produced` exactly as pr-claim's mid-run stale_selection exit does. The
// superseded outcome itself is the journaled workspace_head_moved marker.
func supersededRunResult(state string, upstream map[string]apiv1.ResultEnvelope, steps int) RunResult {
	return RunResult{Status: StatusCompleted, FinalState: state, Outputs: upstream, Steps: steps}
}

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
// attempt's recorded failure type (ClassifyDispatchFailure). Each dispatch still
// carries an explicit RetryPolicy{MaximumAttempts: 1} (stageActivityOptions)
// so the unlimited default is structurally unreachable.
// deltaOut, when non-nil, receives the WINNING attempt's number and whatever
// workspace delta it published (#3763, #3803), so the walk can append it to
// the continuity record. It is an out-param rather than part of the returned
// envelope because it is a fact about the attempt loop, not the stage's
// result: a retried attempt's bundle describes a workspace that was thrown
// away with its pod or worktree, and only the winner's may be carried.
func dispatchWithRetry(ctx workflow.Context, in RunInput, t apiv1.Task, rec *runJournal, pointers []apiv1.ContextPointer, dispatch func(workflow.Context, int, journal.AttemptClass, *usageallowance.Allowance) (stageActivityResult, error), deltaOut *deltaPublication) (apiv1.ResultEnvelope, error) {
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
	maxAttempts := policyMaxAttempts + runsemantics.DefaultMaxInfrastructureAttempts - 1

	var usageBudget runsemantics.StageUsageBudget
	var policyAttempts, infrastructureFailures, priorAttemptDeferrals int32
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
		policyBefore := policyAttempts
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
		rec.stageStarted(startedAt, t, int(attempt), class)
		rec.bindChildStart(ctx, t, int(attempt), class)
		if merr := rec.contextManifest(startedAt, t.Name, int(attempt), class, rec.childContextPointers(t.Name, pointers)); merr != nil {
			return apiv1.ResultEnvelope{}, merr
		}
		var res apiv1.ResultEnvelope
		var err error
		var identity *attemptidentity.Identity
		emitErr := rec.emitPending(ctx)
		if emitErr == nil {
			var activityResult stageActivityResult
			dispatchCtx := withDispatchAttemptStarted(ctx, startedAt)
			step := childStep{Attempt: int(attempt), Class: class, PolicyAttempts: policyBefore, InfrastructureFailures: infrastructureFailures, Pointers: pointers}
			activityResult, err = rec.dispatchChargedAttempt(dispatchCtx, in, t, step, &usageBudget, dispatch)
			res = activityResult.ResultEnvelope
			identity = activityResult.AttemptIdentity
			if stopsAttemptLoop(ctx, err) {
				return apiv1.ResultEnvelope{}, err
			}
			if errors.Is(err, errChildContinued) {
				// A yield continues this policy attempt: no retry allowance is consumed.
				maxAttempts, policyAttempts, nextRetryClass = maxAttempts+1, policyBefore, class
				continue
			}
			if err != nil && activityResult.Placement == nil {
				activityResult.Placement = DispatchFailurePlacement(err)
			}
			// Placement provenance for THIS attempt, before anything the
			// attempt's outcome decides (#3875). Journaled for a failed
			// dispatch too when the dispatch reported one, so the record of
			// where an attempt ran does not depend on whether it succeeded.
			rec.placement(ctx, t.Name, int(attempt), class, activityResult)
			rec.recordDeferredRunBranch(ctx, err, res, len(activityResult.Mutations) > 0)

			if t.Type == apiv1.TaskAgentic && t.Limits != nil && (t.Limits.MaxTokens > 0 || t.Limits.MaxCostUSD > 0) && workflow.GetVersion(ctx, "cumulative-agentic-usage", workflow.DefaultVersion, 1) != workflow.DefaultVersion {
				usage := dispatchFailureUsage(err)
				if activityResult.Usage != nil {
					usage = *activityResult.Usage
				}
				limits := apiv1.Limits{}
				if t.Limits != nil {
					limits = *t.Limits
				}
				// Worker-loss timeouts may hide an attempt that already spent
				// budget. Require usage in that case even though the activity
				// could not report it; pre-execution failures remain retryable.
				usageBudget.Apply(limits, usage.Metrics, usageRequired(usage, err), &res, &err)
			}
			if err == nil {
				if rejection := unsupportedRevisionResult(res, t.Type); rejection != nil {
					rec.workspaceRevisionRefused(ctx, t.Name, int(attempt), class, rejection, identity)
					return apiv1.ResultEnvelope{}, rejection
				}
				res.WorkspaceRevision = nil
				res.Artifacts = normalizeArtifactIntegrity(t.Type, res.Artifacts)
				res = enforceExpectedOutputs(ctx, in.DSLVersion, t, res)
				// Attempt-scoped implementation-lane artifacts (#3882),
				// committed BEFORE stage.finished exactly where the local
				// runner's dispatchTask commits them. The ordering is
				// deliberate on both lanes: the capture protects work against
				// everything that runs after the attempt, so it must not be
				// sequenced behind the attempt's own outcome.
				if aerr := recordAttemptArtifacts(ctx, rec, in, t.Name, int(attempt), class, activityResult); aerr != nil {
					return apiv1.ResultEnvelope{}, aerr
				}
				err = declaredArtifactRetryError(ctx, t.Type, res, policyAttempts < policyMaxAttempts)
			}
			if err == nil {
				rec.mutationIssues(ctx, t.Name, int(attempt), class, activityResult.MutationIssues)
				rec.mutations(ctx, t.Name, int(attempt), class, activityResult.Mutations)
				rec.stageFinished(ctx, t.Name, int(attempt), class, res, t.ContinueOnError, identity)
				emitErr = rec.emitPending(ctx)
				if emitErr == nil {
					// Only a WINNING attempt's delta is carried forward. A
					// retried attempt's bundle describes a workspace that was
					// thrown away with its pod, and building the next stage on
					// it would resurrect abandoned work.
					if deltaOut != nil {
						*deltaOut = deltaPublication{
							Attempt:   int(attempt),
							Digest:    activityResult.WorkspaceDelta,
							Base:      activityResult.WorkspaceDeltaBase,
							Tip:       activityResult.WorkspaceDeltaTip,
							Unchanged: activityResult.WorkspaceDeltaUnchanged,

							PushedBranches: pushedBranches(activityResult.Mutations),
						}
					}
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
			if infrastructureFailures >= runsemantics.DefaultMaxInfrastructureAttempts {
				rec.exhaustedTerminalRetry(t.Name, journal.AttemptInfra, int(infrastructureFailures), int(runsemantics.DefaultMaxInfrastructureAttempts))
				return apiv1.ResultEnvelope{}, fmt.Errorf(
					"engine: journal stage %q: %w (attempt %d/%d)",
					t.Name, lastErr, infrastructureFailures, runsemantics.DefaultMaxInfrastructureAttempts)
			}
			if retryDelay := infrastructureRetryDelay(emitErr, backoff, workflow.Now(ctx)); retryDelay > 0 {
				if serr := sleepBeforeRetry(ctx, rec, t.Name, int(attempt), nextRetryClass, retryDelay); serr != nil {
					return apiv1.ResultEnvelope{}, serr
				}
			}
			continue
		}
		lastErr = err
		if rejection := workspaceRevisionRejection(err); rejection != nil {
			rec.workspaceRevisionRefused(ctx, t.Name, int(attempt), class, rejection, attemptIdentityFromError(err))
			return apiv1.ResultEnvelope{}, rejection
		}
		failureClass, cerr := classifyAttemptFailure(ctx, t, err)
		if cerr != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute stage %q: %w", t.Name, cerr)
		}
		if identity == nil {
			identity = attemptIdentityFromError(err)
		}
		rec.executorError(ctx, t.Name, int(attempt), class, failureClass, err, identity)
		if isWorkerLoss(err) && len(t.PolicyActions) > 0 {
			return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute side-effecting stage %q: refusing to retry after worker loss: %w", t.Name, err)
		}

		retryLimit, retryCount := policyMaxAttempts, policyAttempts
		shouldRetry := policyAttempts < policyMaxAttempts
		nextRetryClass = journal.AttemptPolicy
		if failureClass == journal.AttemptInfra {
			chargeInfraFailure(err, &infrastructureFailures, &priorAttemptDeferrals, &maxAttempts)
			retryLimit, retryCount = runsemantics.DefaultMaxInfrastructureAttempts, infrastructureFailures
			shouldRetry = infrastructureFailures < runsemantics.DefaultMaxInfrastructureAttempts
			nextRetryClass = journal.AttemptInfra
		}
		if !shouldRetry {
			rec.exhaustedTerminalRetry(t.Name, failureClass, int(retryCount), int(retryLimit))
			return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute stage %q: %w (attempt %d/%d)", t.Name, lastErr, retryCount, retryLimit)
		}
		retryDelay := infrastructureRetryDelay(err, backoff, workflow.Now(ctx))
		if retryDelay > 0 {
			if serr := sleepBeforeRetry(ctx, rec, t.Name, int(attempt), nextRetryClass, retryDelay); serr != nil {
				return apiv1.ResultEnvelope{}, serr
			}
		}
	}
	// Unreachable: maxAttempts >= 1 always executes the loop body at least
	// once, and every path inside either returns or continues.
	return apiv1.ResultEnvelope{}, fmt.Errorf("engine: execute stage %q: exhausted attempts: %w", t.Name, lastErr)
}

func usageRequired(usage AttemptUsage, err error) bool {
	return usage.Reported || usage.Unavailable || isWorkerLoss(err)
}

func attemptIdentityFromError(err error) *attemptidentity.Identity {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var appErr *temporal.ApplicationError
		if !errors.As(current, &appErr) || !appErr.HasDetails() {
			continue
		}
		var identity attemptidentity.Identity
		if appErr.Details(&identity) == nil && identity.WorkerIdentity != "" {
			return &identity
		}
		var retryAt time.Time
		if appErr.Details(&retryAt, &identity) == nil && identity.WorkerIdentity != "" {
			return &identity
		}
	}
	return nil
}

func infrastructureRetryDelay(err error, backoff time.Duration, now time.Time) time.Duration {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var appErr *temporal.ApplicationError
		if !errors.As(current, &appErr) || appErr.Type() != FailureTypeInfrastructure || !appErr.HasDetails() {
			continue
		}
		var retryAt time.Time
		if appErr.Details(&retryAt) != nil {
			var identity attemptidentity.Identity
			if appErr.Details(&retryAt, &identity) != nil {
				continue
			}
		}
		until := retryAt.Sub(now)
		if until > backoff {
			return until
		}
		return backoff
	}
	return backoff
}

func nextAttemptUsageAllowance(
	task apiv1.Task,
	budget *runsemantics.StageUsageBudget,
) *usageallowance.Allowance {
	if task.Type != apiv1.TaskAgentic || task.Limits == nil ||
		(task.Limits.MaxTokens <= 0 && task.Limits.MaxCostUSD <= 0) {
		return nil
	}
	allowance := budget.Allowance(*task.Limits)
	return &allowance
}

// ClassifyDispatchFailure maps one failed dispatch to the journal attempt
// class its retry would consume, derived purely from the error shape Temporal
// records in history — no side-channel state, so the projection (#629) can
// re-derive the identical classes.
//
// EXPORTED for decision 003 ruling 2: the daemon's runner blocks on a
// DispatchOne workflow and gets back the SAME error shapes this reads
// (Temporal surfaces the activity's application error and its timeouts
// through the workflow's own failure), so it must classify the attempt with
// this function rather than a second copy. Two copies is how the runner and
// the engine would start disagreeing about which retries cost the policy
// budget — the exact drift D15 names.
//
// The classes:
//
//   - an application error typed FailureTypeInfrastructure is infrastructure;
//   - any other application error is policy (the local runner's
//     dispatchRetryFailureClass rule: everything unmarked is policy-driven).
//     A stage overrunning its declared duration limit lands here: the worker
//     self-enforces the limit and surfaces it as invoke.Timeout →
//     FailureTypeStage, the same policy class the local runner assigns (#724);
//   - a Temporal ScheduleToStart timeout is infrastructure because the
//     activity never began. StartToClose and Heartbeat are policy-classed
//     because the worker may have been lost after the stage committed an
//     external effect; a task
//     declaring policyActions is therefore stopped before retry. The stage
//     retry loop alone reclassifies a heartbeat loss on a stage without
//     policyActions as infrastructure (classifyAttemptFailure, #6750);
//   - anything else fails closed as unclassifiable. A projection error, never
//     a silent default to "infra".
func ClassifyDispatchFailure(err error) (journal.AttemptClass, error) {
	if errors.Is(err, errHandoffWorkerLost) {
		return journal.AttemptInfra, nil
	}
	var timeoutErr *temporal.TimeoutError
	if errors.As(err, &timeoutErr) {
		switch timeoutErr.TimeoutType() {
		case enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START:
			return journal.AttemptInfra, nil
		case enumspb.TIMEOUT_TYPE_START_TO_CLOSE, enumspb.TIMEOUT_TYPE_HEARTBEAT:
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

// recordAttemptArtifacts commits the three attempt-scoped artifacts an
// activity can report back (#3882): the #724 salvage marker, the #813
// base-sync conflict detail, and the #3366 unpushed diff with its sidecar.
//
// All three are recorded from BYTES the activity returned rather than written
// by the activity, so the projection stays a pure function of history (#629)
// and the repair projection can rebuild them from a completed workflow it
// never observed live.
func recordAttemptArtifacts(
	ctx workflow.Context,
	rec *runJournal,
	in RunInput,
	stage string,
	attempt int,
	class journal.AttemptClass,
	result stageActivityResult,
) error {
	if err := rec.recordSalvage(ctx, stage, attempt, class, result.SalvageMarker); err != nil {
		return fmt.Errorf("engine: journal salvage marker for stage %q: %w", stage, err)
	}
	if err := rec.recordBaseSyncConflict(ctx, stage, attempt, class, result.BaseSyncConflict); err != nil {
		return fmt.Errorf("engine: journal base-sync conflict detail for stage %q: %w", stage, err)
	}
	if err := rec.recordUnpushedDiff(ctx, in, stage, attempt, class, result.UnpushedDiff); err != nil {
		return fmt.Errorf("engine: journal unpushed diff for stage %q: %w", stage, err)
	}
	return nil
}

// dispatchChargedAttempt dispatches one attempt under its usage allowance
// and routes a trusted child handoff through admission.
func (r *runJournal) dispatchChargedAttempt(ctx workflow.Context, in RunInput, t apiv1.Task, step childStep, budget *runsemantics.StageUsageBudget,
	dispatch func(workflow.Context, int, journal.AttemptClass, *usageallowance.Allowance) (stageActivityResult, error)) (stageActivityResult, error) {
	allowance := nextAttemptUsageAllowance(t, budget)
	step.Charge = childHandoffCharge(t, budget)
	return r.dispatchWithChildAdmission(ctx, in, t, step, func(c workflow.Context, a int, cl journal.AttemptClass) (stageActivityResult, error) {
		return dispatch(c, a, cl, allowance)
	})
}

// childHandoffCharge applies the stage's cumulative usage cap to a yielding
// attempt before its child runs, as the runner charges before it waits.
func childHandoffCharge(t apiv1.Task, budget *runsemantics.StageUsageBudget) func(stageActivityResult) error {
	return func(result stageActivityResult) error {
		if t.Type != apiv1.TaskAgentic || t.Limits == nil || (t.Limits.MaxTokens <= 0 && t.Limits.MaxCostUSD <= 0) {
			return nil
		}
		usage := AttemptUsage{}
		if result.Usage != nil {
			usage = *result.Usage
		}
		envelope := result.ResultEnvelope
		var err error
		budget.Apply(*t.Limits, usage.Metrics, usage.Reported, &envelope, &err)
		if err == nil && envelope.Error != nil && envelope.Error.Code == runsemantics.BudgetExceededErrorCode {
			err = errors.New("engine: cumulative parent usage exhausted while handing off child")
		}
		return err
	}
}
