package engine

import (
	"errors"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
)

// childWaitError must not enter the ordinary stage retry path: admission or
// custody may already have committed. Activity retries use the original intent;
// exhausted reconciliation leaves the family stopped, not a fresh invocation.
type childWaitError struct{ error }

func (e childWaitError) Unwrap() error { return e.error }

func (r *runJournal) bindChildStart(ctx workflow.Context, t apiv1.Task, continuation bool) {
	if ctx.Value(childProtocolContextKey{}) == true && t.ChildWorkflows != nil {
		r.proj.Ops[len(r.proj.Ops)-1].ChildStageStart = &livejournal.ChildStageStart{Continuation: continuation}
	}
}

func (r *runJournal) committedChildReceipt(stage string) *livejournal.ChildOriginReceipt {
	for i := len(r.proj.Ops) - 1; i >= 0; i-- {
		op := r.proj.Ops[i]
		if op.Event != nil && op.Event.Type == journal.EventStageStarted && op.Event.Stage == stage {
			receipt, ok := r.childOrigins[op.EmitKey]
			if !ok {
				return nil
			}
			return &receipt
		}
	}
	return nil
}

func (r *runJournal) committedChildOrigin(stage string) *apiv1.ChildWorkflowOrigin {
	receipt := r.committedChildReceipt(stage)
	if receipt == nil {
		return nil
	}
	origin := receipt.Origin
	return &origin
}

// dispatchWithChildWait retains retry and usage budgets across suspended stage
// sessions. Continuations get a fresh committed attempt ID within the original
// occurrence; no policy retry is charged just for parking.
func dispatchWithChildWait(ctx workflow.Context, in RunInput, t apiv1.Task, rec *runJournal, attempt *int32, class journal.AttemptClass, policyAttempts, infrastructureFailures int32, usage *runner.StageUsageBudget, pointers []apiv1.ContextPointer, dispatch func(workflow.Context, int, journal.AttemptClass) (stageActivityResult, error)) (stageActivityResult, error) {
	if ctx.Value(childProtocolContextKey{}) != true {
		return dispatch(ctx, int(*attempt), class)
	}
	for {
		result, err := dispatch(ctx, int(*attempt), class)
		if result.ChildWait == nil {
			return result, err
		}
		if err != nil {
			return stageActivityResult{}, childWaitError{errors.Join(errors.New("engine: ambiguous child handoff"), err)}
		}
		if ctx.Value(childProtocolContextKey{}) != true || t.ChildWorkflows == nil {
			return stageActivityResult{}, childWaitError{errors.New("engine: child handoff is disabled")}
		}
		origin := rec.committedChildOrigin(t.Name)
		if origin == nil {
			return stageActivityResult{}, childWaitError{errors.New("engine: child handoff has no committed origin")}
		}
		// The count lives in deterministic run state and survives ordinary retries.
		if rec.childCounts == nil {
			rec.childCounts = map[string]int32{}
		}
		if rec.childCounts[origin.StageOccurrence] >= t.ChildWorkflows.EffectiveMaxChildren() || rec.childTotal >= apiv1.MaxChildWorkflows {
			return stageActivityResult{}, childWaitError{errors.New("engine: stage child limit exceeded")}
		}
		rec.childCounts[origin.StageOccurrence]++
		rec.childTotal++
		binding, err := admitEngineChild(ctx, in, t.Name, *result.ChildWait, origin)
		if err != nil {
			return stageActivityResult{}, childWaitError{err}
		}
		if t.Limits != nil && (t.Limits.MaxTokens > 0 || t.Limits.MaxCostUSD > 0) {
			metrics := AttemptUsage{}
			if result.Usage != nil {
				metrics = *result.Usage
			}
			envelope := result.ResultEnvelope
			usage.Apply(*t.Limits, metrics.Metrics, metrics.Reported, &envelope, &err)
			if envelope.Error != nil && envelope.Error.Code == runner.BudgetExceededErrorCode {
				err = errors.New("engine: cumulative parent usage exhausted while handing off child")
			}
			if err != nil {
				owned, cancel := workflow.NewDisconnectedContext(ctx)
				defer cancel()
				cleanup := workflow.ExecuteActivity(childActivityContext(owned), "CancelEngineChild", binding).Get(owned, nil)
				return stageActivityResult{}, childWaitError{errors.Join(err, cleanup)}
			}
		}
		rec.append(ctx, journal.Event{Type: journal.EventRunnerAnnotation, Stage: t.Name, Attempt: int(*attempt), AttemptClass: class, Runner: map[string]any{"kind": journal.ChildWaitKind, "childWait": journal.ChildWaitHeader{Version: 1, ParentRunID: in.RunID, Request: binding.Request, PolicyAttempts: policyAttempts, InfrastructureFailures: infrastructureFailures}}})
		if err = rec.emitPending(ctx); err != nil {
			owned, cancel := workflow.NewDisconnectedContext(ctx)
			defer cancel()
			cleanup := workflow.ExecuteActivity(childActivityContext(owned), "CancelEngineChild", binding).Get(owned, nil)
			return stageActivityResult{}, childWaitError{errors.Join(err, cleanup)}
		}
		pointer, err := waitEngineChild(ctx, binding)
		if err != nil {
			return stageActivityResult{}, childWaitError{err}
		}
		rec.append(ctx, journal.Event{Type: journal.EventRunnerAnnotation, Stage: t.Name, Attempt: int(*attempt), AttemptClass: class, Runner: map[string]any{"kind": journal.ChildContinuedKind, "requestId": binding.Request.RequestID, "context": pointer}})
		(*attempt)++
		rec.stageStarted(workflow.Now(ctx), t, int(*attempt), class)
		rec.bindChildStart(ctx, t, true)
		if rec.childContinuations == nil {
			rec.childContinuations = map[string][]apiv1.ContextPointer{}
		}
		rec.childContinuations[origin.StageOccurrence] = append(rec.childContinuations[origin.StageOccurrence], pointer)
		contextPointers := append(append([]apiv1.ContextPointer(nil), pointers...), rec.childContinuations[origin.StageOccurrence]...)
		if err = rec.contextManifest(workflow.Now(ctx), t.Name, int(*attempt), class, contextPointers); err != nil {
			return stageActivityResult{}, childWaitError{err}
		}
		if err = rec.emitPending(ctx); err != nil {
			owned, cancel := workflow.NewDisconnectedContext(ctx)
			defer cancel()
			cleanup := workflow.ExecuteActivity(childActivityContext(owned), "CancelEngineChild", binding).Get(owned, nil)
			return stageActivityResult{}, childWaitError{errors.Join(err, cleanup)}
		}
	}
}
