package engine

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

const staticSelectionVersion = "engine-static-selection-v1"

func (r *runJournal) selectiveFanOutPlan(
	ctx workflow.Context,
	in RunInput,
	m *wf.Machine,
	parallel apiv1.Parallel,
) (fanOutPlan, error) {
	plan := staticFanOutPlan(parallel)
	if parallel.Selection == nil {
		return plan, nil
	}
	source, limit, err := r.fanOutSource(m, parallel.Selection.Source)
	if err != nil {
		return fanOutPlan{}, err
	}
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Millisecond,
			MaximumAttempts: 2,
		},
	})
	var frozen FreezeParallelSelectionResult
	err = workflow.ExecuteActivity(activityCtx, ActFreezeParallelSelection, FreezeParallelSelectionInput{
		RunID: in.RunID, ParallelName: parallel.Name, Definition: parallel,
		Source: source, MaxSourceBytes: limit,
	}).Get(activityCtx, &frozen)
	if err != nil {
		return fanOutPlan{}, err
	}
	selection := frozen.Selection
	plan.Started.Selection = &selection
	applyFrozenSelection(&plan, selection)
	return plan, nil
}

func applyFrozenSelection(plan *fanOutPlan, selection journal.ParallelSelection) {
	selected := make(map[string]bool, len(selection.Selected))
	for _, name := range selection.Selected {
		selected[name] = true
	}
	skipReason := fmt.Sprintf("not selected by %s.%s", selection.Source.Task, selection.Source.Slot)
	for i := range plan.Branches {
		branch := &plan.Branches[i]
		if !branch.Optional {
			continue
		}
		if selected[branch.Name] {
			branch.Reason = selection.Reasons[branch.Name]
			continue
		}
		branch.Reason = skipReason
		branch.PreSettled = &journal.BranchOutcome{
			Branch: branch.ID, Name: branch.Name,
			Status: journal.BranchSkipped, Reason: skipReason,
		}
	}
}
