package engine

import (
	"errors"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	wf "github.com/goobers/goobers/internal/workflow"
)

const childWorkflowChangeID = "engine-child-workflows-v1"

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
