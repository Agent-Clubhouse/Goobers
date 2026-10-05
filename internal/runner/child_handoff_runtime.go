package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

type childYield struct {
	request   ChildHandoffRequest
	workspace *stageWorkspace
	record    childWaitRecord
}

func (*childYield) Error() string { return "runner: invocation yielded workspace custody for a child" }

type childHandoffObserved struct {
	request ChildHandoffRequest
	err     error
}
type childInvocationResult struct {
	result apiv1.ResultEnvelope
	err    error
}

var errChildWaitDrain = errors.New("runner: child wait retained during shutdown")

// invokeWithChildHandoff is the actual invocation owner. A durable request
// cancels the runtime, then waits for its return and every writer's independent
// acknowledgement. An MCP response never supplies that acknowledgement.
func (r *Runner) invokeWithChildHandoff(ctx context.Context, tf taskFrame, invocation *gooberInvocation, env apiv1.InvocationEnvelope, workspace *stageWorkspace) (result apiv1.ResultEnvelope, retErr error) {
	if tf.in.Child != nil {
		return invokeChildAgent(ctx, tf, invocation, env)
	}
	if tf.t.ChildWorkflows == nil || r.cfg.ChildHandoff == nil {
		return invocation.Invoke(ctx, env)
	}
	if tf.in.pinnedWorkspace != nil || workspace.worktree == nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("runner: child handoff requires a managed repository workspace")
	}
	releaseHold, err := r.holdContainedParentWorkspace(ctx, tf, workspace, env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer func() { retErr = errors.Join(retErr, releaseHold()) }()
	owned, cancel := context.WithCancel(ctx)
	defer cancel()
	owned, proof := invoke.WithWorkspaceQuiescence(owned)
	observed := make(chan childHandoffObserved, 1)
	go func() {
		request, err := r.cfg.ChildHandoff.Await(owned, env)
		observed <- childHandoffObserved{request, err}
	}()
	finished := make(chan childInvocationResult, 1)
	go func() { result, err := invocation.Invoke(owned, env); finished <- childInvocationResult{result, err} }()
	var observation childHandoffObserved
	var output childInvocationResult
	select {
	case output = <-finished:
		cancel()
		observation = <-observed
		if observation.err != nil {
			if !errors.Is(observation.err, context.Canceled) {
				return output.result, errors.Join(output.err, observation.err)
			}
			return output.result, output.err
		}
	case observation = <-observed:
		cancel()
		output = <-finished
	}
	if observation.err != nil {
		return output.result, errors.Join(output.err, observation.err)
	}
	if observation.request.ParentRunID != env.RunID || observation.request.Gaggle != env.Gaggle {
		return output.result, fmt.Errorf("runner: child handoff belongs to another parent")
	}
	if err := observation.request.validate(env.ChildWorkflowOrigin); err != nil {
		return output.result, err
	}
	if err := proof.Verify(); err != nil {
		return output.result, err
	}
	custody, err := workspace.worktree.HoldForChild(context.WithoutCancel(ctx))
	if err != nil {
		return output.result, err
	}
	yield := &childYield{request: observation.request, workspace: workspace, record: childWaitRecord{Version: 1, ParentRunID: env.RunID, Request: observation.request, Workspace: &custody, Context: env.ContextPointers, Transcript: output.result.Transcript, InstructionAddendum: env.InstructionAddendum}}
	// Cleanup releases auxiliary leases but cannot delete the parent checkout.
	workspace.retainedChild = func(context.Context) error { return nil }
	return output.result, yield
}

func (r *Runner) waitForChild(ctx context.Context, tf *taskFrame, attempt int, class journal.AttemptClass, yielded *childYield, policyAttempts, infrastructureFailures int32, cumulative *stageUsageTotals) error {
	record := yielded.record
	record.PolicyAttempts, record.InfrastructureFailures = policyAttempts, infrastructureFailures
	record.Usage, record.InvalidCost = maps.Clone(cumulative.metrics), cumulative.invalidCost
	if cumulative.costUSD != nil {
		record.CostUSD = cumulative.costUSD.RatString()
	}
	event, err := childWaitEvent(tf.t.Name, attempt, class, record)
	if err != nil {
		return err
	}
	if err := publishChildWait(ctx, tf, event); err != nil {
		return err
	}
	err = r.continueChildWait(ctx, tf, attempt, class, record, yielded.workspace)
	if err != nil && ctx.Err() != nil {
		return errors.Join(errChildWaitDrain, err)
	}
	return err
}

func (r *Runner) continueChildWait(ctx context.Context, tf *taskFrame, attempt int, class journal.AttemptClass, record childWaitRecord, workspace *stageWorkspace) error {
	if r.cfg.ChildHandoff == nil || r.cfg.ChildParentCapacity == nil {
		return fmt.Errorf("runner: child wait requires host custody and capacity services")
	}
	if tf.in.parallelChild != nil {
		if err := r.continueParallelChildWait(ctx, tf, attempt, class, record, workspace); err != nil {
			if ctx.Err() != nil {
				return errors.Join(errChildWaitDrain, err)
			}
			return err
		}
		return nil
	}
	dispositionIssue, err := r.yieldChildCustody(ctx, tf, attempt, class, record.Request, ChildWorkspaceCustody{Path: workspace.path, RepoRef: tf.in.RepoRef})
	if err != nil {
		if ctx.Err() != nil {
			return errors.Join(errChildWaitDrain, ctx.Err())
		}
		return err
	}
	suspension, err := r.cfg.ChildParentCapacity.SuspendChildParent(ctx, tf.in.RunID)
	if err != nil {
		return err
	}
	completion, err := r.cfg.ChildHandoff.Wait(ctx, record.Request)
	if err != nil {
		if ctx.Err() != nil {
			return errors.Join(errChildWaitDrain, ctx.Err())
		}
		return err
	}
	completion.DispositionIssue = dispositionIssue
	pointer, err := recordChildCompletion(tf, attempt, class, record, completion)
	if err != nil {
		return err
	}
	if err := resumeChildCapacity(ctx, suspension, tf, attempt, class); err != nil {
		if ctx.Err() != nil {
			return errors.Join(errChildWaitDrain, ctx.Err())
		}
		return err
	}
	// Only a reacquired parent may close the marker. A crash before this event
	// remains parked; a crash afterwards restores the retained checkout.
	if err := tf.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: tf.t.Name, Attempt: attempt, AttemptClass: class, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": record.Request.RequestID, "context": pointer}}); err != nil {
		return err
	}
	return restoreChildContext(tf, record, pointer, workspace)
}

func restoreChildContext(tf *taskFrame, record childWaitRecord, pointer apiv1.ContextPointer, workspace *stageWorkspace) error {
	tf.upstream = append(append([]apiv1.ContextPointer(nil), record.Context...), pointer)
	if record.Transcript != nil {
		tf.upstream = append(tf.upstream, apiv1.ContextPointer{Name: "child-parent-transcript", Artifact: record.Transcript, Integrity: record.Transcript.Integrity})
	}
	tf.heldChildWorkspace = workspace
	return apiv1.ValidateInputIntegrity(tf.in.Item, tf.upstream, tf.t.MinimumIntegrity)
}

func (r *Runner) restoreChildWait(ctx context.Context, tf *taskFrame, cumulative *stageUsageTotals) error {
	record := tf.childWaitResume
	if record.ParentRunID != tf.in.RunID || record.Workspace == nil || record.Workspace.OwnerRunID != tf.in.RunID || r.cfg.Worktrees == nil || r.cfg.RepoCloneURL == nil {
		return fmt.Errorf("runner: child wait workspace custody is unavailable")
	}
	repoURL, err := r.cfg.RepoCloneURL(tf.in.RepoRef)
	if err != nil {
		return err
	}
	wt, err := r.cfg.Worktrees.AdoptHeldStage(ctx, repoURL, *record.Workspace)
	if err != nil {
		return err
	}
	workspace := &stageWorkspace{path: wt.Path, worktree: wt, retainedChild: func(context.Context) error { return nil }}
	cumulative.metrics, cumulative.invalidCost = maps.Clone(record.Usage), record.InvalidCost
	if cumulative.metrics == nil {
		cumulative.metrics = make(map[string]float64)
	}
	if record.CostUSD != "" {
		var ok bool
		cumulative.costUSD, ok = new(big.Rat).SetString(record.CostUSD)
		if !ok || cumulative.costUSD.Sign() < 0 {
			return fmt.Errorf("runner: invalid child wait usage custody")
		}
	}
	if tf.childWaitCompletion != nil {
		return restoreChildContext(tf, *record, *tf.childWaitCompletion, workspace)
	}
	return r.continueChildWait(ctx, tf, tf.childWaitAttempt, tf.childWaitClass, *record, workspace)
}

func finishChildStageCustody(ctx context.Context, tf *taskFrame) error {
	if tf.heldChildWorkspace == nil || tf.heldChildWorkspace.parentContribution {
		return nil
	}
	workspace := tf.heldChildWorkspace
	if err := workspace.worktree.ReleaseChildHold(ctx); err != nil {
		return err
	}
	workspace.retainedChild = nil
	return workspace.Remove(ctx)
}

func resumeChildCapacity(ctx context.Context, suspension ChildParentSuspension, tf *taskFrame, attempt int, class journal.AttemptClass) error {
	if suspension == nil {
		return fmt.Errorf("runner: child capacity suspension is missing")
	}
	lastReason := ""
	for {
		err := suspension.Resume(ctx)
		if err == nil {
			return nil
		}
		var pending *ChildCapacityWaitError
		if !errors.As(err, &pending) {
			return fmt.Errorf("runner: parent continuation cannot reacquire custody: %w", err)
		}
		if pending.Reason != lastReason {
			if err := tf.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: tf.t.Name, Attempt: attempt, AttemptClass: class, Runner: map[string]any{"kind": "child.workflow.capacity-blocked", "reason": boundFailureMessage(pending.Reason), "policyBlocked": pending.PolicyBlocked}}); err != nil {
				return err
			}
			lastReason = pending.Reason
		}
		delay := time.Second
		if pending.PolicyBlocked {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func recordChildCompletion(tf *taskFrame, attempt int, class journal.AttemptClass, record childWaitRecord, completion ChildHandoffCompletion) (apiv1.ContextPointer, error) {
	if len(completion.DispositionIssue) > 1024 || len(completion.Summary) > 16<<10 || len(completion.References) > 64 || len(completion.ResultRef) > 1024 || len(completion.WorkspaceRef) > 1024 {
		return apiv1.ContextPointer{}, fmt.Errorf("runner: child completion exceeds its bound")
	}
	data, err := json.Marshal(struct {
		Request    ChildHandoffRequest    `json:"request"`
		Completion ChildHandoffCompletion `json:"completion"`
	}{record.Request, completion})
	if err != nil || len(data) > maxChildWaitBytes {
		return apiv1.ContextPointer{}, fmt.Errorf("runner: child completion exceeds its artifact bound")
	}
	ref, err := tf.jr.RecordStageArtifact(tf.t.Name, attempt, class, fmt.Sprintf("child-%d-completion.json", attempt), data)
	if err != nil {
		return apiv1.ContextPointer{}, err
	}
	return apiv1.ContextPointer{Name: "child-completion", Integrity: apiv1.IntegrityUnapproved, Artifact: &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "application/json", Integrity: apiv1.IntegrityUnapproved}}, nil
}

func admitTaskIntegrity(tf taskFrame) error {
	jr, in, t := tf.jr, tf.in, tf.t
	upstream, upstreamResult, completed, fanIn := tf.upstream, tf.upstreamResult, tf.completed, tf.fanIn
	// Both admission checks run here, before any workspace or credential
	// provisioning below. contextFrom-selected pointers are graded by
	// ValidateInputIntegrity; inputsFrom values are bare scalars whose only
	// provenance is the stage that produced them, so they are graded separately
	// against the same minimum. Checking only the former let a stage exclude an
	// unapproved producer's artifact with contextFrom and still import that
	// producer's provider-authored text through inputsFrom (TBH-4).
	integrityErr := apiv1.ValidateInputIntegrity(in.Item, upstream, t.MinimumIntegrity)
	if integrityErr == nil {
		integrityErr = apiv1.ValidateResolvedInputIntegrity(
			resolvedInputGrades(t, in.Machine, upstreamResult, completed, fanIn), t.MinimumIntegrity)
	}
	if err := integrityErr; err != nil {
		admission := &apiv1.IntegrityAdmissionError{}
		if !errors.As(err, &admission) {
			return err
		}
		if appendErr := jr.Append(journal.Event{
			Type:             journal.EventError,
			Stage:            t.Name,
			Integrity:        admission.Actual,
			MinimumIntegrity: admission.Minimum,
			Error:            journal.ErrorDetailFor(apiv1.IntegrityAdmissionErrorCode, admission),
		}); appendErr != nil {
			return fmt.Errorf("runner: journal integrity refusal for %q: %w", t.Name, appendErr)
		}
		return fmt.Errorf("runner: refuse stage %q: %w", t.Name, admission)
	}
	return nil
}

func takeChildTaskResume(ws *walkState, stage string) (*resumeContext, error) {
	if ws.resume == nil || ws.resume.stage != stage {
		return nil, nil
	}
	if ws.resume.childWaitErr != nil {
		return nil, ws.resume.childWaitErr
	}
	if ws.resume.childWait == nil {
		return nil, nil
	}
	restored := ws.resume
	if !restored.childWaitRunning {
		ws.resume = nil
	}
	return restored, nil
}

func pureChildYield(err error) (*childYield, bool) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var yielded *childYield
		for _, part := range joined.Unwrap() {
			candidate, ok := pureChildYield(part)
			if !ok || yielded != nil && yielded != candidate {
				return nil, false
			}
			yielded = candidate
		}
		return yielded, yielded != nil
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return pureChildYield(wrapped.Unwrap())
	}
	var yielded *childYield
	ok := errors.As(err, &yielded)
	return yielded, ok
}
