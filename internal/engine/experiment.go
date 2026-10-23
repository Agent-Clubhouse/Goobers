package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/backlogdefaults"
	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

const experimentVersion = "task-experiment-v1"

type taskExperimentState struct {
	stage      string
	occurrence int64
	config     bandit.Config
	prepare    func(bandit.Assignment) (map[string]any, error)
	inputs     map[string]any
	plan       ExperimentPlan
}

func (r *runJournal) configureExperimentTask(ctx workflow.Context, in RunInput, task apiv1.Task, finalInputs map[string]any) error {
	r.experiment = nil
	if task.Experiment == nil {
		return nil
	}
	if workflow.GetVersion(ctx, experimentVersion, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return ErrExperimentUnsupported
	}
	config, _, err := bandit.ConfigForTask(r.machine, task)
	if err != nil {
		return err
	}
	base, err := wf.TaskInvocationInputs(r.machine, task)
	if err != nil {
		return err
	}
	base = backlogdefaults.Apply(task, base, in.BacklogQueryAssignedTo, in.BacklogQueryRequireLabels)
	base = backlogdefaults.ApplyIssueOwnershipScope(base, in.IssueOwnershipAssignees, in.IssueOwnershipUnassigned)
	bindings, err := r.branchBindings(task.Name)
	if err != nil {
		return err
	}
	r.experiment = &taskExperimentState{
		stage: task.Name, occurrence: int64(len(r.proj.Ops) + 1), config: config,
		prepare: func(assignment bandit.Assignment) (map[string]any, error) {
			assigned := assignment.Apply(base)
			inputs := make(map[string]any, len(assigned))
			for key, value := range assigned {
				inputs[key] = value
			}
			// Variant settings precede inputsFrom, branch bindings and enforced
			// backlog scope, exactly as on the runner. Each retry gets a new map.
			for _, key := range sortedKeys(task.InputsFrom) {
				inputs[key] = finalInputs[key]
			}
			bindings.apply(inputs)
			r.staticJoinInputs(task.Name, inputs)
			return backlogdefaults.ApplyBacklogScopeToInvocation(task, inputs, in.BacklogQueryBacklogLabels, in.BacklogQueryLabelPredicate)
		},
	}
	return nil
}

func experimentActivityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: 30 * time.Second, ScheduleToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 10 * time.Second, MaximumAttempts: 5},
	})
}

func (r *runJournal) beginExperimentAttempt(ctx workflow.Context, runID string) error {
	state := r.experiment
	if state == nil {
		return nil
	}
	input := ExperimentPlanInput{RunID: runID, Config: state.config}
	for _, op := range r.projectionSnapshot().Ops {
		if op.Event != nil && op.Event.Type == journal.EventBanditRetired && op.Event.Stage == state.stage {
			if arm, ok := op.Event.Outputs["arm"].(string); ok {
				input.PriorRetired = append(input.PriorRetired, arm)
			}
		}
	}
	activityCtx := experimentActivityContext(ctx)
	if err := workflow.ExecuteLocalActivity(activityCtx, actPrepareExperiment, input).Get(activityCtx, &state.plan); err != nil {
		return fmt.Errorf("prepare task %q experiment: %w", state.stage, err)
	}
	inputs, err := state.prepare(state.plan.Assignment)
	if err != nil {
		return err
	}
	state.inputs = maps.Clone(inputs)
	return r.appendExperimentEvents(ctx, state.plan.AssignmentEvents)
}

func (r *runJournal) finishExperimentAttempt(ctx workflow.Context, runID string, attempt int, result apiv1.ResultEnvelope) error {
	state := r.experiment
	if state == nil {
		return nil
	}
	outcome := state.plan.Failure
	if result.Status == apiv1.ResultSuccess {
		outcome = state.plan.Success
	}
	record := bandit.ObservationRecord{
		Key:         bandit.ObservationKey{RunID: runID, Stage: state.stage, Branch: r.branch, Occurrence: state.occurrence, Attempt: attempt},
		Observation: outcome.Observation,
	}
	activityCtx := experimentActivityContext(ctx)
	if err := workflow.ExecuteLocalActivity(activityCtx, actRecordExperiment, record).Get(activityCtx, nil); err != nil {
		return fmt.Errorf("record task %q experiment: %w", state.stage, err)
	}
	return r.appendExperimentEvents(ctx, outcome.Events)
}

func (r *runJournal) appendExperimentEvents(ctx workflow.Context, events []json.RawMessage) error {
	for _, raw := range events {
		var event journal.Event
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&event); err != nil {
			return err
		}
		if event.Type == journal.EventBanditRetired && r.experimentRetirementRecorded(event) {
			continue
		}
		r.append(ctx, event)
	}
	return nil
}

func (r *runJournal) experimentRetirementRecorded(event journal.Event) bool {
	for _, op := range r.projectionSnapshot().Ops {
		if op.Event != nil && op.Event.Type == event.Type && op.Event.Stage == event.Stage && op.Event.Outputs["arm"] == event.Outputs["arm"] {
			return true
		}
	}
	return false
}
