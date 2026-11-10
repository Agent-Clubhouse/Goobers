package engine

import (
	"errors"

	"go.temporal.io/sdk/workflow"

	wf "github.com/goobers/goobers/internal/workflow"
)

const childWorkflowChangeID = "engine-child-workflows-v1"

// runWithChildWorkflows is internal enablement, registered only by test
// workers under Run's name. The marker precedes every new command. Replaying
// an unmarked legacy input preserves the original refusal/command sequence,
// including permissive nil preview semantics for ordinary runs. The production
// entry and worker registration stay refused until qualification (#7758).
func runWithChildWorkflows(ctx workflow.Context, in RunInput) (RunResult, error) {
	if in.FrozenChild == nil && wf.RefuseChildWorkflowExecution(in.Spec) == nil {
		return run(ctx, in)
	}
	if workflow.GetVersion(ctx, childWorkflowChangeID, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return Run(ctx, in)
	}
	if in.PreviewFeaturesEnabled == nil || !*in.PreviewFeaturesEnabled || !in.LiveJournal {
		return RunResult{}, errors.New("engine: children require explicit preview and live journal")
	}
	return Run(workflow.WithValue(ctx, childProtocolContextKey{}, true), in)
}
