package engine

// Generated-child execution (HA-P4-09): the parent parks, starts the frozen
// child as a Temporal child workflow of Run, and continues the same stage
// occurrence with the bounded result. Reachable only under the protocol
// context; production Run refuses frozen inputs until #7758 qualifies.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// childExecutionChangeID gates every command after admission. Histories
// without it keep the HA-P4-08 fence: admit, cancel, stop.
const childExecutionChangeID = "engine-child-execution-v1"

// errChildContinued tells the attempt loop that this attempt yielded to a
// child that settled; the occurrence continues with a fresh attempt ID and no
// retry allowance is consumed, exactly as the runner continues after a yield.
var errChildContinued = errors.New("engine: stage continues after its child")

// ChildService adds parking, terminal custody and continuation to admission.
// Park stops/joins parent writers, snapshots to blobs, durably parks the parent
// after verifying the committed wait marker, and releases its capacity slot.
// Finalize joins child writers and retains a sealed result; its state is
// verified against journal/custody, not trusted from input. Complete verifies
// result refs, preserves pending disposition, and reacquires the parent slot
// before returning its bounded continuation artifact.
type ChildService interface {
	ChildAdmissions
	Park(context.Context, ChildParkInput) error
	Finalize(context.Context, ChildFinalizeInput) (childresult.TerminalReturn, error)
	Complete(context.Context, ChildCompletionInput) (apiv1.ContextPointer, error)
}

// ChildParkInput binds parking to the acknowledged wait marker's identity.
type ChildParkInput struct {
	Binding ChildRunBinding           `json:"binding"`
	Wait    livejournal.AppendReceipt `json:"wait"`
}

// ChildFinalizeInput requests sealed custody using execution pins and journal time.
type ChildFinalizeInput struct {
	Binding    ChildRunBinding         `json:"binding"`
	State      triggerqueue.ChildState `json:"state"`
	FinishedAt time.Time               `json:"finishedAt"`
}

// ChildCompletionInput binds a bounded terminal return to its waiting parent.
type ChildCompletionInput struct {
	Binding ChildRunBinding            `json:"binding"`
	Wait    livejournal.AppendReceipt  `json:"wait"`
	Result  childresult.TerminalReturn `json:"result"`
}

func validWaitReceipt(r livejournal.AppendReceipt) bool {
	return r.Seq > 0 && r.Key != ""
}

// ParkEngineChild confirms parent writer custody before releasing capacity.
func (a *Activities) ParkEngineChild(ctx context.Context, in ChildParkInput) error {
	if a.EngineChildren == nil {
		return ErrNotConfigured
	}
	if err := in.Binding.validate(); err != nil || !validWaitReceipt(in.Wait) {
		return errors.Join(errors.New("engine: invalid child park"), err)
	}
	return a.EngineChildren.Park(ctx, in)
}

// FinalizeEngineChild retains stopped-writer custody and bounds the return.
func (a *Activities) FinalizeEngineChild(ctx context.Context, in ChildFinalizeInput) (childresult.TerminalReturn, error) {
	if a.EngineChildren == nil {
		return childresult.TerminalReturn{}, ErrNotConfigured
	}
	if err := in.Binding.validate(); err != nil {
		return childresult.TerminalReturn{}, err
	}
	out, err := a.EngineChildren.Finalize(ctx, in)
	if err != nil {
		return out, err
	}
	if err = out.Validate(in.Binding.Child); err != nil {
		return childresult.TerminalReturn{}, err
	}
	if out.Input.State != in.State {
		return childresult.TerminalReturn{}, errors.New("engine: finalizer changed terminal state")
	}
	return out, nil
}

// CompleteEngineChild reacquires capacity and returns a retained context reference.
func (a *Activities) CompleteEngineChild(ctx context.Context, in ChildCompletionInput) (apiv1.ContextPointer, error) {
	if a.EngineChildren == nil {
		return apiv1.ContextPointer{}, ErrNotConfigured
	}
	if err := in.Binding.validate(); err != nil || !validWaitReceipt(in.Wait) {
		return apiv1.ContextPointer{}, errors.Join(errors.New("engine: invalid child completion"), err)
	}
	if err := in.Result.Validate(in.Binding.Child); err != nil {
		return apiv1.ContextPointer{}, err
	}
	out, err := a.EngineChildren.Complete(ctx, in)
	if err != nil {
		return out, err
	}
	data, e := json.Marshal(out)
	if e != nil || len(data) > 4096 || out.Artifact == nil || !blobstore.ValidDigest(out.Artifact.Digest) || out.Artifact.Size <= 0 || out.Artifact.Size > 64<<10 || out.External != nil || out.RunID != "" || out.Validate() != nil {
		return apiv1.ContextPointer{}, errors.New("engine: invalid child continuation artifact")
	}
	// The child is untrusted data for its parent regardless of what it claims.
	out.Integrity = apiv1.IntegrityUnapproved
	out.Artifact.Integrity = apiv1.IntegrityUnapproved
	return out, nil
}

func childWorkflowID(b ChildRunBinding) string {
	// Gaggle scoping prevents unrelated tenants with the same business run ID
	// from contending for the same Temporal execution name.
	data, _ := json.Marshal([]string{b.Child.Identity.Gaggle, b.Child.RunID})
	return "engine-child/" + journal.Digest(data)
}

// childStep is the attempt-loop state a handoff needs. Charge applies the
// stage's cumulative usage cap to the yielding attempt before the child runs.
type childStep struct {
	Attempt                int
	Class                  journal.AttemptClass
	PolicyAttempts         int32
	InfrastructureFailures int32
	Pointers               []apiv1.ContextPointer
	Charge                 func(stageActivityResult) error
}

// commitChildLifecycle appends one serial lifecycle event and returns the
// writer's acknowledged per-op identity for it.
func (r *runJournal) commitChildLifecycle(ctx workflow.Context, event journal.Event) (livejournal.AppendReceipt, error) {
	r.append(ctx, event)
	i := len(r.proj.Ops) - 1
	r.proj.Ops[i].Receipt = true
	if err := r.emitPending(ctx); err != nil {
		return livejournal.AppendReceipt{}, err
	}
	receipt, ok := r.receipts[r.proj.Ops[i].EmitKey]
	if !ok {
		return livejournal.AppendReceipt{}, errors.New("engine: child lifecycle identity not acknowledged")
	}
	return receipt, nil
}

// preserveSerialReceipts keeps per-op receipts for a walk without a shared
// branch owner, using the owner's validation unchanged.
func (r *runJournal) preserveSerialReceipts(ops []livejournal.Op, receipts []livejournal.AppendReceipt, watermark uint64) error {
	ledger := journalCommitOwner{receipts: r.receipts}
	if err := ledger.preserveAppendReceipts(ops, receipts, watermark); err != nil {
		return err
	}
	r.receipts = ledger.receipts
	return nil
}

// awaitChild journals the wait, parks, runs the child and journals the
// continuation. Any failure after admission settles custody through Cancel.
func (r *runJournal) awaitChild(ctx workflow.Context, in RunInput, t apiv1.Task, step childStep, yielded stageActivityResult, binding ChildRunBinding) error {
	if err := step.Charge(yielded); err != nil {
		return cancelChild(ctx, binding, err)
	}
	header := journal.ChildWaitHeader{Version: 1, ParentRunID: in.RunID, Request: binding.Request, PolicyAttempts: step.PolicyAttempts, InfrastructureFailures: step.InfrastructureFailures}
	wait, err := r.commitChildLifecycle(ctx, journal.Event{Type: journal.EventRunnerAnnotation, Stage: t.Name, Attempt: step.Attempt, AttemptClass: step.Class,
		Runner: map[string]any{"kind": journal.ChildWaitKind, "childWait": header}})
	if err != nil {
		return cancelChild(ctx, binding, err)
	}
	pointer, err := waitEngineChild(ctx, binding, wait)
	if err != nil {
		return err
	}
	if _, err = r.commitChildLifecycle(ctx, journal.Event{Type: journal.EventRunnerAnnotation, Stage: t.Name, Attempt: step.Attempt, AttemptClass: step.Class,
		Runner: map[string]any{"kind": journal.ChildContinuedKind, "requestId": binding.Request.RequestID, "context": pointer}}); err != nil {
		return err
	}
	continuation := []apiv1.ContextPointer{pointer}
	if yielded.Transcript != nil {
		continuation = append(continuation, apiv1.ContextPointer{Name: "child-parent-transcript", Artifact: yielded.Transcript, Integrity: yielded.Transcript.Integrity})
	}
	r.children.continuations[t.Name] = append(r.children.continuations[t.Name], continuation...)
	if err := apiv1.ValidateInputIntegrity(in.Item, r.childContextPointers(t.Name, step.Pointers), t.MinimumIntegrity); err != nil {
		var admission *apiv1.IntegrityAdmissionError
		if errors.As(err, &admission) {
			r.integrityRefused(ctx, t.Name, admission)
		}
		return err
	}
	return errChildContinued
}

// childContextPointers is the occurrence's effective context: the stage's
// own inputs followed by every settled child return in admission order.
func (r *runJournal) childContextPointers(stage string, pointers []apiv1.ContextPointer) []apiv1.ContextPointer {
	if r.children == nil || len(r.children.continuations[stage]) == 0 {
		return pointers
	}
	return append(append([]apiv1.ContextPointer(nil), pointers...), r.children.continuations[stage]...)
}

// childResultIntegrity grades a continued attempt no higher than the weakest
// child return it consumed.
func (r *runJournal) childResultIntegrity(stage string, produced apiv1.Integrity) apiv1.Integrity {
	if r.children == nil {
		return produced
	}
	for _, pointer := range r.children.continuations[stage] {
		produced = apiv1.WeakestIntegrity(produced, pointer.Integrity)
	}
	return produced
}

// waitEngineChild parks before starting the graph. The owning context stays
// alive until child cancellation and custody cleanup settle. ParentClosePolicy
// is the backstop if the parent is terminated externally.
func waitEngineChild(ctx workflow.Context, b ChildRunBinding, wait livejournal.AppendReceipt) (pointer apiv1.ContextPointer, err error) {
	owned, disconnect := workflow.NewDisconnectedContext(ctx)
	defer disconnect()
	cleanup := func(cause error) (apiv1.ContextPointer, error) {
		cleanupErr := workflow.ExecuteActivity(childActivityContext(owned), "CancelEngineChild", b).Get(owned, nil)
		return apiv1.ContextPointer{}, errors.Join(cause, cleanupErr)
	}
	if err = workflow.ExecuteActivity(childActivityContext(ctx), "ParkEngineChild", ChildParkInput{Binding: b, Wait: wait}).Get(ctx, nil); err != nil {
		return cleanup(err)
	}
	if ctx.Err() != nil {
		return cleanup(ctx.Err())
	}
	remaining := b.Deadline.Sub(workflow.Now(ctx))
	if remaining <= 0 {
		return cleanup(errors.New("engine: child wait deadline expired"))
	}
	childCtx, cancelChild := workflow.WithCancel(owned)
	defer cancelChild()
	childCtx = workflow.WithChildOptions(childCtx, workflow.ChildWorkflowOptions{WorkflowID: childWorkflowID(b), ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
		WaitForCancellation: true, WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionTimeout: remaining + childCleanupTimeout,
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}})
	enabled := true
	future := workflow.ExecuteChildWorkflow(childCtx, Run, RunInput{RunID: b.Child.RunID, Gaggle: b.Child.Identity.Gaggle, FrozenChild: &b, PreviewFeaturesEnabled: &enabled, LiveJournal: true})
	if err = awaitChildFuture(ctx, owned, future, cancelChild, remaining); err != nil {
		return cleanup(err)
	}
	var result RunResult
	if err = future.Get(owned, &result); err != nil {
		return cleanup(err)
	}
	if result.ChildResult == nil {
		return cleanup(errors.New("engine: child returned no retained result"))
	}
	if err = result.ChildResult.Validate(b.Child); err != nil {
		return cleanup(err)
	}
	if !workflow.Now(ctx).Before(b.Deadline) {
		return cleanup(errors.New("engine: child continuation deadline expired"))
	}
	if err = workflow.ExecuteActivity(childActivityContext(ctx), "CompleteEngineChild", ChildCompletionInput{Binding: b, Wait: wait, Result: *result.ChildResult}).Get(ctx, &pointer); err != nil {
		return cleanup(err)
	}
	if !workflow.Now(ctx).Before(b.Deadline) {
		return cleanup(errors.New("engine: capacity reacquisition exceeded child deadline"))
	}
	return pointer, nil
}

// awaitChildFuture returns nil once the child settles inside its deadline.
// Expiry or parent cancellation cancels the child and joins its disconnected
// finalizer for a bounded time; it never permits continuation.
func awaitChildFuture(ctx, owned workflow.Context, future workflow.ChildWorkflowFuture, cancelChild func(), remaining time.Duration) error {
	timerCtx, cancelTimer := workflow.WithCancel(owned)
	defer cancelTimer()
	timer := workflow.NewTimer(timerCtx, remaining)
	var stopErr error
	selector := workflow.NewSelector(owned)
	selector.AddFuture(future, func(workflow.Future) {})
	selector.AddFuture(timer, func(workflow.Future) { stopErr = errors.New("engine: child wait deadline expired") })
	selector.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) { stopErr = ctx.Err() })
	selector.Select(owned)
	if stopErr == nil && ctx.Err() != nil {
		stopErr = ctx.Err()
	}
	if stopErr == nil {
		return nil
	}
	cancelChild()
	// Lost workers must retain custody for service reconciliation, never keep
	// a parent open forever or permit continuation.
	joinCtx, stopJoin := workflow.WithCancel(owned)
	defer stopJoin()
	join := workflow.NewSelector(owned)
	join.AddFuture(future, func(workflow.Future) {})
	join.AddFuture(workflow.NewTimer(joinCtx, childCleanupTimeout), func(workflow.Future) {
		stopErr = errors.Join(stopErr, errors.New("engine: child cancellation settlement timed out"))
	})
	join.Select(owned)
	return stopErr
}

// runFrozenChild executes an admitted child from its verified manifest and
// always returns through the trusted finalizer, success or not.
func runFrozenChild(ctx workflow.Context, in RunInput) (RunResult, error) {
	if !childProtocolEnabled(ctx) {
		return RunResult{}, errors.New("engine: generated child execution remains disabled")
	}
	if in.Spec.Start != "" || len(in.Spec.Tasks) != 0 {
		return RunResult{}, errors.New("engine: generated child input is ambiguous")
	}
	binding := *in.FrozenChild
	if err := binding.validate(); err != nil {
		return RunResult{}, err
	}
	info := workflow.GetInfo(ctx)
	if info.ParentWorkflowExecution == nil || info.ParentWorkflowExecution.ID != binding.ParentWorkflowID || info.ParentWorkflowExecution.RunID != binding.ParentTemporalRunID || info.WorkflowExecution.ID != childWorkflowID(binding) {
		return RunResult{}, errors.New("engine: child execution lineage mismatch")
	}
	var frozen RunInput
	if err := workflow.ExecuteActivity(childActivityContext(ctx), "LoadEngineChild", binding).Get(ctx, &frozen); err != nil {
		return RunResult{}, err
	}
	r := binding.Request
	frozen.childLineage = &journal.ChildLineage{Gaggle: r.Gaggle, ParentRunID: r.ParentRunID, ParentWorkflow: binding.ParentWorkflow, StageOccurrence: r.Origin.StageOccurrence,
		InvocationKey: r.InvocationKey, AcceptanceID: r.AcceptanceID, SourceDigest: r.SourceDigest, EnvelopeDigest: binding.ManifestDigest}
	// A generated child never acquires child authority of its own.
	result, runErr := run(workflow.WithValue(ctx, childProtocolContextKey{}, false), frozen)
	state := triggerqueue.ChildCompleted
	if runErr != nil || result.Status != StatusCompleted {
		state = triggerqueue.ChildFailed
	}
	if temporal.IsCanceledError(runErr) || ctx.Err() != nil {
		state = triggerqueue.ChildCancelled
	}
	owned, disconnect := workflow.NewDisconnectedContext(ctx)
	defer disconnect()
	var terminal childresult.TerminalReturn
	if err := workflow.ExecuteActivity(childActivityContext(owned), "FinalizeEngineChild", ChildFinalizeInput{Binding: binding, State: state, FinishedAt: workflow.Now(ctx)}).Get(owned, &terminal); err != nil {
		return RunResult{}, err
	}
	// Failure details and stage outputs live in the sealed result artifact.
	settled := RunResult{Status: StatusCompleted, Steps: result.Steps}
	if state != triggerqueue.ChildCompleted {
		settled.Status = StatusFailed
	}
	return settled.WithChildResult(binding.Child, terminal)
}
