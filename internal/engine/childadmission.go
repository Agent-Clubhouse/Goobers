package engine

import (
	"errors"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// errChildExecutionUnavailable marks the HA-P4-08 activation boundary: a
// verified admission is fenced and retained, but no child is started.
var errChildExecutionUnavailable = errors.New("engine: generated child execution is not available on this worker")

// childWaitError must not enter the ordinary stage retry path: admission or
// custody may already have committed. Activity retries use the original intent;
// exhausted reconciliation leaves the family stopped, not a fresh invocation.
type childWaitError struct{ error }

func (e childWaitError) Unwrap() error { return e.error }

// childFamily is deterministic run state rebuilt from recorded activity results.
// Counts survive ordinary retries and bound history before continuation support.
type childFamily struct {
	total       int32
	occurrences map[string]int32
}

// childAdmissionPlan is the single descriptor the admission controller accepts.
// Origin is the writer's committed per-op receipt for the active stage start,
// never a projection ordinal or batch watermark.
type childAdmissionPlan struct {
	Task   apiv1.Task
	Origin livejournal.ChildOriginReceipt
	Input  ChildAdmissionInput
}

func (p childAdmissionPlan) validate(in RunInput) error {
	request, origin := p.Input.Request, p.Origin
	if p.Task.Type != apiv1.TaskAgentic || p.Task.ChildWorkflows == nil {
		return errors.New("engine: stage does not declare child workflows")
	}
	if origin.Seq == 0 || origin.Stage != p.Task.Name || origin.Branch != 0 || p.Input.Stage != p.Task.Name || p.Input.OriginSeq != origin.Seq {
		return errors.New("engine: child handoff has no committed origin")
	}
	if err := request.Validate(&origin.Origin); err != nil {
		return err
	}
	if request.Action != "wait" || request.Gaggle != in.Gaggle || request.ParentRunID != in.RunID {
		return errors.New("engine: child request crossed parent authority")
	}
	return nil
}

// bindChildStart asks the journal writer to bind child authority to the stage
// start just recorded, using the runner's continuation rule. It is inert unless
// the version-gated child protocol is active, and refuses branch walks whose
// shared owner does not return child-origin receipts.
func (r *runJournal) bindChildStart(ctx workflow.Context, t apiv1.Task, attempt int, class journal.AttemptClass) {
	if !childProtocolEnabled(ctx) || t.Type != apiv1.TaskAgentic || t.ChildWorkflows == nil || r.owner != nil || !r.live {
		return
	}
	continuation := attempt > 1 || class == journal.AttemptHuman || class == journal.AttemptInfra
	r.proj.Ops[len(r.proj.Ops)-1].ChildStageStart = &livejournal.ChildStageStart{Continuation: continuation}
}

// committedChildReceipt returns the acknowledged origin for this exact stage
// attempt, or nil when the start was not bound or not yet acknowledged.
func (r *runJournal) committedChildReceipt(stage string, attempt int) *livejournal.ChildOriginReceipt {
	for i := len(r.proj.Ops) - 1; i >= 0; i-- {
		op := r.proj.Ops[i]
		if op.Event == nil || op.Event.Type != journal.EventStageStarted || op.Event.Stage != stage {
			continue
		}
		receipt, ok := r.childOrigins[op.EmitKey]
		if op.ChildStageStart == nil || !ok || op.Event.Attempt != attempt || receipt.Attempt != attempt {
			return nil
		}
		return &receipt
	}
	return nil
}

// childAttemptEnvelope carries the committed origin into dispatch exactly as
// the runner's pinPublicationAuthority does, including the writer-assigned
// publication visit that replaced the projection ordinal at commit.
func (r *runJournal) childAttemptEnvelope(env apiv1.InvocationEnvelope, task apiv1.Task) apiv1.InvocationEnvelope {
	receipt := r.committedChildReceipt(task.Name, int(env.Attempt))
	if receipt == nil {
		return env
	}
	origin := receipt.Origin
	env.ChildWorkflowOrigin = &origin
	env.Goober = task.Goober
	if env.ArtifactPublication != nil {
		env.ArtifactPublication.Visit = receipt.Seq
	}
	return env
}

// childAdmissionPlanFor builds the descriptor from committed workflow state.
// The adapter's request names the origin it saw; it must equal the receipt.
func (r *runJournal) childAdmissionPlanFor(ctx workflow.Context, in RunInput, t apiv1.Task, attempt int, request journal.ChildHandoffRequest) (childAdmissionPlan, error) {
	receipt := r.committedChildReceipt(t.Name, attempt)
	if receipt == nil {
		return childAdmissionPlan{}, errors.New("engine: child handoff has no committed origin")
	}
	info := workflow.GetInfo(ctx)
	plan := childAdmissionPlan{Task: t, Origin: *receipt, Input: ChildAdmissionInput{
		Request: request, ParentWorkflow: in.WorkflowName, Stage: t.Name, OriginSeq: receipt.Seq,
		ParentWorkflowID: info.WorkflowExecution.ID, ParentTemporalRunID: info.WorkflowExecution.RunID,
	}}
	return plan, plan.validate(in)
}

// reserveChildSlot charges the per-occurrence and per-run bounds before the
// admission command, so a refused admission still consumes no external slot.
func (r *runJournal) reserveChildSlot(plan childAdmissionPlan) error {
	if r.children == nil {
		r.children = &childFamily{occurrences: map[string]int32{}}
	}
	occurrence := plan.Origin.Origin.StageOccurrence
	if r.children.occurrences[occurrence] >= plan.Task.ChildWorkflows.EffectiveMaxChildren() || r.children.total >= apiv1.MaxChildWorkflows {
		return errors.New("engine: stage child limit exceeded")
	}
	r.children.occurrences[occurrence]++
	r.children.total++
	return nil
}

// admitChild is the controller entry point: one validated descriptor, one
// idempotent admission command, and a receipt checked against the intent.
func (r *runJournal) admitChild(ctx workflow.Context, plan childAdmissionPlan) (ChildRunBinding, error) {
	if err := r.reserveChildSlot(plan); err != nil {
		return ChildRunBinding{}, err
	}
	var binding ChildRunBinding
	if err := workflow.ExecuteActivity(childActivityContext(ctx), "AdmitEngineChild", plan.Input).Get(ctx, &binding); err != nil {
		return binding, err
	}
	if err := binding.validate(); err != nil {
		return binding, err
	}
	if !binding.sameParent(plan.Input) {
		return binding, errors.New("engine: admission receipt differs from parent intent")
	}
	return binding, nil
}

// cancelChild fences an admitted family from a disconnected context, so
// workflow cancellation cannot skip custody cleanup.
func cancelChild(ctx workflow.Context, binding ChildRunBinding, cause error) error {
	owned, disconnect := workflow.NewDisconnectedContext(ctx)
	defer disconnect()
	return errors.Join(cause, workflow.ExecuteActivity(childActivityContext(owned), "CancelEngineChild", binding).Get(owned, nil))
}

// dispatchWithChildAdmission routes a trusted adapter's child handoff through
// admission. Outside the protocol a handoff fails closed; inside it, phase
// HA-P4-08 admits, fences and stops before any child command.
func (r *runJournal) dispatchWithChildAdmission(ctx workflow.Context, in RunInput, t apiv1.Task, attempt int, class journal.AttemptClass, dispatch func(workflow.Context, int, journal.AttemptClass) (stageActivityResult, error)) (stageActivityResult, error) {
	result, err := dispatch(ctx, attempt, class)
	if result.ChildWait == nil {
		return result, err
	}
	if err != nil {
		return stageActivityResult{}, childWaitError{errors.Join(errors.New("engine: ambiguous child handoff"), err)}
	}
	if !childProtocolEnabled(ctx) {
		return stageActivityResult{}, childWaitError{errors.New("engine: child handoff is disabled")}
	}
	plan, err := r.childAdmissionPlanFor(ctx, in, t, attempt, *result.ChildWait)
	if err != nil {
		return stageActivityResult{}, childWaitError{err}
	}
	binding, err := r.admitChild(ctx, plan)
	if err != nil {
		return stageActivityResult{}, childWaitError{err}
	}
	return stageActivityResult{}, childWaitError{cancelChild(ctx, binding, errChildExecutionUnavailable)}
}
