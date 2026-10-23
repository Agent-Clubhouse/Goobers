package engine

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fanout"
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
	source, limit, err := r.selectionSource(m, parallel.Selection.Source)
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

func (r *runJournal) selectionSource(
	m *wf.Machine,
	source apiv1.FanOutSource,
) (journal.FanOutSourceRef, int64, error) {
	task, ok := m.Task(source.Task)
	if !ok {
		return journal.FanOutSourceRef{}, 0, fmt.Errorf("engine: selection producer %q is not a task", source.Task)
	}
	slotIndex := -1
	limit := fanout.MaxSourceBytes
	for i, slot := range task.ArtifactSlots {
		if slot.Name != source.Slot {
			continue
		}
		slotIndex = i
		if slot.MaxSize > 0 && slot.MaxSize < limit {
			limit = slot.MaxSize
		}
		break
	}
	if slotIndex < 0 {
		return journal.FanOutSourceRef{}, 0, fmt.Errorf("engine: selection source slot %s.%s is not declared", source.Task, source.Slot)
	}
	projection := r.projectionSnapshot()
	for i := len(projection.Ops) - 1; i >= 0; i-- {
		finished := projection.Ops[i].Event
		if finished == nil || finished.Branch != 0 || finished.Type != journal.EventStageFinished ||
			finished.Stage != source.Task {
			continue
		}
		if finished.Status != string(apiv1.ResultSuccess) {
			return journal.FanOutSourceRef{}, 0, fmt.Errorf(
				"engine: selection producer %q attempt %d finished %q, not success",
				source.Task, finished.Attempt, finished.Status,
			)
		}
		if slotIndex >= len(finished.Artifacts) {
			return journal.FanOutSourceRef{}, 0, fmt.Errorf(
				"engine: selection source slot %s.%s has no journaled artifact",
				source.Task, source.Slot,
			)
		}
		for j := i - 1; j >= 0; j-- {
			started := projection.Ops[j].Event
			if started == nil || started.Branch != 0 || started.Type != journal.EventStageStarted ||
				started.Stage != source.Task || started.Attempt != finished.Attempt {
				continue
			}
			visit, _ := started.Runner["artifactVisit"].(uint64)
			if visit == 0 {
				return journal.FanOutSourceRef{}, 0, fmt.Errorf(
					"engine: selection producer %q has no artifact publication visit",
					source.Task,
				)
			}
			ref := finished.Artifacts[slotIndex]
			return journal.FanOutSourceRef{
				Task: source.Task, Slot: source.Slot, Visit: visit, Attempt: finished.Attempt,
				Digest: ref.Digest, Integrity: ref.Integrity, Path: ref.Path,
				Size: ref.Size, MediaType: ref.MediaType,
			}, limit, nil
		}
		return journal.FanOutSourceRef{}, 0, fmt.Errorf(
			"engine: selection producer %q attempt %d has no matching start",
			source.Task, finished.Attempt,
		)
	}
	return journal.FanOutSourceRef{}, 0, fmt.Errorf(
		"engine: selection producer %q has no settled root attempt",
		source.Task,
	)
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
