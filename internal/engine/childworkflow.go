package engine

// The ECW-2 protocol is deliberately unreachable from Run/RegisterWith. Tests
// register runWithChildWorkflows under Run's workflow name. Activating a worker
// requires the durable service adapter and ECW-3 qualification, not an input flag.

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
	"github.com/goobers/goobers/internal/triggerqueue"
	wf "github.com/goobers/goobers/internal/workflow"
)

const childWorkflowChangeID = "engine-child-workflows-v1"
const childCleanupTimeout = 5 * time.Minute
const maxChildManifestBytes = 512 << 10

type childProtocolContextKey struct{}

// ChildRunBinding is an immutable admission receipt, never agent-provided
// authority. The service fences every operation by acceptance and parent epoch.
// ManifestDigest addresses the entire frozen input, not a mutable catalog name.
type ChildRunBinding struct {
	Request             journal.ChildHandoffRequest `json:"request"`
	Child               triggerqueue.ChildRecord    `json:"child"`
	ManifestDigest      string                      `json:"manifestDigest"`
	ParentWorkflowID    string                      `json:"parentWorkflowId"`
	ParentWorkflow      string                      `json:"parentWorkflow"`
	ParentTemporalRunID string                      `json:"parentTemporalRunId"`
	Deadline            time.Time                   `json:"deadline"`
}

// ChildAdmissionInput is built from the committed origin and owning execution.
// Request is a dispatch adapter's verified handoff; the service rechecks authority.
type ChildAdmissionInput struct {
	Request             journal.ChildHandoffRequest `json:"request"`
	ParentWorkflowID    string                      `json:"parentWorkflowId"`
	ParentWorkflow      string                      `json:"parentWorkflow"`
	ParentTemporalRunID string                      `json:"parentTemporalRunId"`
	Stage               string                      `json:"stage"`
}

// FrozenChildManifest is stored once by admission. Repository/configuration,
// grants, kit and placement pins travel inside Input. No workspace bytes do.
type FrozenChildManifest struct {
	Version      int      `json:"version"`
	SourceDigest string   `json:"sourceDigest"`
	Input        RunInput `json:"input"`
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
	Result  childresult.TerminalReturn `json:"result"`
}

// EngineChildService is an activity-side custody boundary. Implementations MUST
// use durable, tenant-scoped compare-and-swap receipts; process-local locks are
// insufficient. Every method is idempotent after lost acknowledgement.
//
// Admit verifies the active origin, policy/preview, exact source bytes and pinned
// placements, and atomically retains admission plus a unique child start intent.
// Park stops/joins parent writers, snapshots to blobs, durably parks the parent,
// and releases its capacity slot before acknowledging. Load reads only the
// digest-addressed manifest. Finalize joins child writers and retains a sealed
// result; its state is verified against journal/custody, not trusted from input.
// Complete verifies result refs, preserves pending disposition, and reacquires
// the parent slot before returning its bounded continuation artifact. Cancel
// durably fences all later starts, settles family writers and retains custody; it must never restore capacity or
// publish a usable snapshot on uncertain termination. Service reconciliation
// owns incomplete operations after these bounded activity retries expire.
// No production implementation is wired by this slice.
type EngineChildService interface {
	Admit(context.Context, ChildAdmissionInput) (ChildRunBinding, error)
	Park(context.Context, ChildRunBinding) error
	Load(context.Context, ChildRunBinding) ([]byte, error)
	Finalize(context.Context, ChildFinalizeInput) (childresult.TerminalReturn, error)
	Complete(context.Context, ChildCompletionInput) (apiv1.ContextPointer, error)
	Cancel(context.Context, ChildRunBinding) error
}

func (b ChildRunBinding) validate() error {
	r, c := b.Request, b.Child
	encoded, err := json.Marshal(b)
	if err != nil || len(encoded) > 16<<10 || r.Validate(&r.Origin) != nil || r.Action != "wait" ||
		c.Identity.Gaggle != r.Gaggle || c.Identity.ParentRunID != r.ParentRunID || c.Identity.StageOccurrence != r.Origin.StageOccurrence || c.Identity.InvocationKey != r.InvocationKey ||
		c.AcceptanceID != r.AcceptanceID || c.RunID != r.ChildRunID || c.ProposalDigest != r.SourceDigest || c.AcceptedAt.IsZero() ||
		!blobstore.ValidDigest(b.ManifestDigest) || b.ParentWorkflow == "" || len(b.ParentWorkflow) > 256 || b.ParentWorkflowID == "" || b.ParentTemporalRunID == "" || !b.Deadline.After(c.AcceptedAt) || b.Deadline.Sub(c.AcceptedAt) > 24*time.Hour {
		return errors.New("engine: invalid frozen child admission")
	}
	return nil
}

// AdmitEngineChild verifies and records one idempotent child start intent.
func (a *Activities) AdmitEngineChild(ctx context.Context, in ChildAdmissionInput) (ChildRunBinding, error) {
	if a.EngineChildren == nil {
		return ChildRunBinding{}, ErrNotConfigured
	}
	out, err := a.EngineChildren.Admit(ctx, in)
	if err != nil {
		return ChildRunBinding{}, err
	}
	if err = out.validate(); err != nil {
		return ChildRunBinding{}, err
	}
	if out.Request != in.Request || out.ParentWorkflow != in.ParentWorkflow || out.ParentWorkflowID != in.ParentWorkflowID || out.ParentTemporalRunID != in.ParentTemporalRunID {
		return ChildRunBinding{}, errors.New("engine: admission changed parent binding")
	}
	return out, nil
}

// ParkEngineChild confirms parent writer custody before releasing capacity.
func (a *Activities) ParkEngineChild(ctx context.Context, in ChildRunBinding) error {
	if a.EngineChildren == nil {
		return ErrNotConfigured
	}
	if err := in.validate(); err != nil {
		return err
	}
	return a.EngineChildren.Park(ctx, in)
}

// LoadEngineChild checks the exact manifest bytes and generated graph pins.
func (a *Activities) LoadEngineChild(ctx context.Context, in ChildRunBinding) (RunInput, error) {
	if a.EngineChildren == nil {
		return RunInput{}, ErrNotConfigured
	}
	if err := in.validate(); err != nil {
		return RunInput{}, err
	}
	data, err := a.EngineChildren.Load(ctx, in)
	if err != nil {
		return RunInput{}, err
	}
	if len(data) > maxChildManifestBytes || journal.Digest(data) != in.ManifestDigest {
		return RunInput{}, errors.New("engine: frozen child manifest digest mismatch")
	}
	var manifest FrozenChildManifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return RunInput{}, err
	}
	child := manifest.Input
	if manifest.Version != 1 || manifest.SourceDigest != in.Request.SourceDigest || child.RunID != in.Child.RunID || child.Gaggle != in.Child.Identity.Gaggle || child.FrozenChild != nil || child.HITL != nil || !child.LiveJournal || child.PreviewFeaturesEnabled == nil || !*child.PreviewFeaturesEnabled || !blobstore.ValidDigest(child.GooberDigest) || !blobstore.ValidDigest(child.ConfigGeneration) {
		return RunInput{}, errors.New("engine: invalid frozen child pins")
	}
	if err = wf.RefuseChildWorkflowExecution(child.Spec); err != nil {
		return RunInput{}, err
	}
	if err = wf.RefuseUnsupportedFanOut(child.Spec); err != nil {
		return RunInput{}, err
	}
	for _, g := range child.Spec.Gates {
		if err = refuseHumanGate(g); err != nil {
			return RunInput{}, err
		}
	}
	digest, err := wf.ComputeDigest(wf.Definition{Name: child.WorkflowName, Version: child.Version, DSLVersion: child.DSLVersion, Spec: child.Spec})
	if err != nil || digest != child.WorkflowDigest {
		return RunInput{}, errors.New("engine: frozen workflow digest mismatch")
	}
	return child, nil
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
	if err := in.Binding.validate(); err != nil {
		return apiv1.ContextPointer{}, err
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
	out.Integrity = apiv1.IntegrityUnapproved
	out.Artifact.Integrity = apiv1.IntegrityUnapproved
	return out, nil
}

// CancelEngineChild fences the family and retains custody without resuming it.
func (a *Activities) CancelEngineChild(ctx context.Context, in ChildRunBinding) error {
	if a.EngineChildren == nil {
		return ErrNotConfigured
	}
	if err := in.validate(); err != nil {
		return err
	}
	return a.EngineChildren.Cancel(ctx, in)
}

func childActivityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute, ScheduleToCloseTimeout: childCleanupTimeout, WaitForCancellation: true, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumAttempts: 3}})
}

// runWithChildWorkflows is internal enablement, called only by test workers.
// The marker precedes every new command. Replaying an unmarked legacy input
// preserves the original refusal/command sequence, including permissive nil
// preview semantics for ordinary runs.
func runWithChildWorkflows(ctx workflow.Context, in RunInput) (RunResult, error) {
	requested := in.FrozenChild != nil || wf.RefuseChildWorkflowExecution(in.Spec) != nil
	if !requested {
		return run(ctx, in)
	}
	if workflow.GetVersion(ctx, childWorkflowChangeID, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return Run(ctx, in)
	}
	if in.PreviewFeaturesEnabled == nil || !*in.PreviewFeaturesEnabled || !in.LiveJournal {
		return RunResult{}, errors.New("engine: children require explicit preview and live journal")
	}
	if in.FrozenChild == nil {
		return run(workflow.WithValue(ctx, childProtocolContextKey{}, true), in)
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
	frozen.childLineage = &journal.ChildLineage{Gaggle: binding.Request.Gaggle, ParentRunID: binding.Request.ParentRunID, ParentWorkflow: binding.ParentWorkflow, StageOccurrence: binding.Request.Origin.StageOccurrence, InvocationKey: binding.Request.InvocationKey, AcceptanceID: binding.Request.AcceptanceID, SourceDigest: binding.Request.SourceDigest, EnvelopeDigest: binding.ManifestDigest}
	result, runErr := run(ctx, frozen)
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
	result = RunResult{Status: StatusCompleted, Steps: result.Steps}
	if state != triggerqueue.ChildCompleted {
		result.Status = StatusFailed
	}
	return result.WithChildResult(binding.Child, terminal)
}

func childWorkflowID(b ChildRunBinding) string {
	// Gaggle scoping prevents unrelated tenants with the same business run ID
	// from contending for the same Temporal execution name.
	data, _ := json.Marshal([]string{b.Child.Identity.Gaggle, b.Child.RunID})
	return "engine-child/" + journal.Digest(data)
}

// waitEngineChild releases capacity before starting the graph. The owning
// context stays alive until child cancellation and custody cleanup settle.
// ParentClosePolicy is the backstop if the parent is terminated externally.
func waitEngineChild(ctx workflow.Context, b ChildRunBinding) (pointer apiv1.ContextPointer, err error) {
	owned, disconnect := workflow.NewDisconnectedContext(ctx)
	defer disconnect()
	cleanup := func(cause error) error {
		cleanupErr := workflow.ExecuteActivity(childActivityContext(owned), "CancelEngineChild", b).Get(owned, nil)
		return errors.Join(cause, cleanupErr)
	}
	if err = workflow.ExecuteActivity(childActivityContext(ctx), "ParkEngineChild", b).Get(ctx, nil); err != nil {
		return pointer, cleanup(err)
	}
	if ctx.Err() != nil {
		return pointer, cleanup(ctx.Err())
	}
	remaining := b.Deadline.Sub(workflow.Now(ctx))
	if remaining <= 0 {
		return pointer, cleanup(errors.New("engine: child wait deadline expired"))
	}
	childCtx, cancelChild := workflow.WithCancel(owned)
	defer cancelChild()
	childCtx = workflow.WithChildOptions(childCtx, workflow.ChildWorkflowOptions{WorkflowID: childWorkflowID(b), ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL, WaitForCancellation: true, WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionTimeout: remaining + childCleanupTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}})
	enabled := true
	future := workflow.ExecuteChildWorkflow(childCtx, Run, RunInput{RunID: b.Child.RunID, Gaggle: b.Child.Identity.Gaggle, FrozenChild: &b, PreviewFeaturesEnabled: &enabled, LiveJournal: true})
	timerCtx, cancelTimer := workflow.WithCancel(owned)
	defer cancelTimer()
	timer := workflow.NewTimer(timerCtx, remaining)
	stopped := false
	selector := workflow.NewSelector(owned)
	selector.AddFuture(future, func(workflow.Future) {})
	selector.AddFuture(timer, func(workflow.Future) { stopped = true; err = errors.New("engine: child wait deadline expired") })
	selector.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) { stopped = true; err = ctx.Err() })
	selector.Select(owned)
	if stopped || ctx.Err() != nil {
		if err == nil {
			err = ctx.Err()
		}
		cancelChild()
		// Wait for the child to run its disconnected terminal finalizer. Bound
		// this join independently: lost workers must retain custody for service
		// reconciliation, never keep a parent open forever or permit continuation.
		joinCtx, stopJoin := workflow.WithCancel(owned)
		joinTimer := workflow.NewTimer(joinCtx, childCleanupTimeout)
		join := workflow.NewSelector(owned)
		join.AddFuture(future, func(workflow.Future) {})
		join.AddFuture(joinTimer, func(workflow.Future) {
			err = errors.Join(err, errors.New("engine: child cancellation settlement timed out"))
		})
		join.Select(owned)
		stopJoin()
		return pointer, cleanup(err)
	}
	cancelTimer()
	var result RunResult
	if err = future.Get(owned, &result); err != nil {
		return pointer, cleanup(err)
	}
	if result.ChildResult == nil {
		return pointer, cleanup(errors.New("engine: child returned no retained result"))
	}
	if err = result.ChildResult.Validate(b.Child); err != nil {
		return pointer, cleanup(err)
	}
	if !workflow.Now(ctx).Before(b.Deadline) {
		return pointer, cleanup(errors.New("engine: child continuation deadline expired"))
	}
	completeCtx := childActivityContext(ctx)
	if err = workflow.ExecuteActivity(completeCtx, "CompleteEngineChild", ChildCompletionInput{Binding: b, Result: *result.ChildResult}).Get(ctx, &pointer); err != nil {
		return pointer, cleanup(err)
	}
	if !workflow.Now(ctx).Before(b.Deadline) {
		return apiv1.ContextPointer{}, cleanup(errors.New("engine: capacity reacquisition exceeded child deadline"))
	}
	return pointer, nil
}

func admitEngineChild(ctx workflow.Context, in RunInput, stage string, request journal.ChildHandoffRequest, origin *apiv1.ChildWorkflowOrigin) (ChildRunBinding, error) {
	if err := request.Validate(origin); err != nil {
		return ChildRunBinding{}, err
	}
	if request.Gaggle != in.Gaggle || request.ParentRunID != in.RunID {
		return ChildRunBinding{}, errors.New("engine: child request crossed parent authority")
	}
	info := workflow.GetInfo(ctx)
	input := ChildAdmissionInput{ParentWorkflow: in.WorkflowName, Request: request, ParentWorkflowID: info.WorkflowExecution.ID, ParentTemporalRunID: info.WorkflowExecution.RunID, Stage: stage}
	var binding ChildRunBinding
	if err := workflow.ExecuteActivity(childActivityContext(ctx), "AdmitEngineChild", input).Get(ctx, &binding); err != nil {
		return binding, err
	}
	if err := binding.validate(); err != nil {
		return binding, err
	}
	if binding.Request != request || binding.ParentWorkflow != input.ParentWorkflow || binding.ParentWorkflowID != input.ParentWorkflowID || binding.ParentTemporalRunID != input.ParentTemporalRunID {
		return binding, errors.New("engine: admission receipt differs from parent intent")
	}
	return binding, nil
}
