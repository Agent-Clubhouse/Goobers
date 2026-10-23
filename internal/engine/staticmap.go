package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runsemantics"
	wf "github.com/goobers/goobers/internal/workflow"
)

const staticMapVersion = "engine-static-map-v1"

func (r *runJournal) staticMap(
	ctx workflow.Context,
	in RunInput,
	machine *wf.Machine,
	def apiv1.Map,
	seed *branchWalkFrame,
) (string, bool, error) {
	if workflow.GetVersion(ctx, staticMapVersion, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return "", true, fmt.Errorf("engine: map execution is refused")
	}
	repeatable := workflow.GetVersion(ctx, staticParallelVisitsVersion, workflow.DefaultVersion, 1) != workflow.DefaultVersion
	if r.staticWalk.seen[def.Name] && !repeatable {
		return "", true, fmt.Errorf("engine: repeated map visit %q is not supported", def.Name)
	}
	if r.owner == nil || r.stepBudget == nil {
		return "", true, fmt.Errorf("engine: map requires commit owner and shared budget")
	}
	if r.staticWalk.seen == nil {
		r.staticWalk.seen = map[string]bool{}
	}
	r.staticWalk.join, r.staticWalk.completeness, r.staticWalk.mapJoin = "", nil, nil
	r.staticWalk.parallel, r.staticWalk.branches = "", nil
	r.staticWalk.seen[def.Name] = true
	plan, err := r.mapFanOutPlan(ctx, in, machine, def)
	if err != nil {
		r.append(ctx, journal.Event{
			Type: journal.EventError, Parallel: def.Name,
			Error: journal.ErrorDetailFor(runsemantics.FanOutSourceInvalidCode, err),
		})
		if emitErr := r.emitPending(ctx); emitErr != nil {
			return "", true, emitErr
		}
		target := def.OnFailure
		if target == "" {
			target = wf.TargetAbort
		}
		return target, true, nil
	}
	_, result, err := r.fanOut(ctx, in, machine, plan, seed)
	if err != nil {
		return "", true, err
	}
	r.staticWalk.join = def.Join
	r.staticWalk.mapJoin = assembleStaticMapJoin(def, plan.Started.MapRoster, result)
	r.staticWalk.currentVisits = repeatable
	return result.Target, true, nil
}

func (r *runJournal) mapFanOutPlan(
	ctx workflow.Context,
	in RunInput,
	machine *wf.Machine,
	def apiv1.Map,
) (fanOutPlan, error) {
	source, sourceLimit, err := r.fanOutSource(machine, def.Source)
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
	var frozen FreezeMapRosterResult
	err = workflow.ExecuteActivity(activityCtx, ActFreezeMapRoster, FreezeMapRosterInput{
		RunID: in.RunID, MapName: def.Name, Definition: def, Source: source,
		MaxSourceBytes: sourceLimit,
	}).Get(activityCtx, &frozen)
	if err != nil {
		return fanOutPlan{}, err
	}
	var loaded LoadFrozenMapRosterResult
	err = workflow.ExecuteActivity(activityCtx, ActLoadFrozenMapRoster, LoadFrozenMapRosterInput{
		RunID: in.RunID, MapName: def.Name, Definition: def,
		RosterDigest: frozen.RosterDigest, RosterSize: frozen.RosterSize,
		Items: frozen.Items,
	}).Get(activityCtx, &loaded)
	if err != nil {
		return fanOutPlan{}, err
	}
	roster := loaded.Roster
	plan := fanOutPlan{
		Name: def.Name, MaxConcurrentBranches: fanout.EffectiveMaxConcurrent(def),
		BranchTimeoutSeconds: def.ItemTimeoutSeconds, FailurePolicy: def.FailurePolicy,
		Join: def.Join, OnFailure: def.OnFailure,
		Started:  journal.Event{Type: journal.EventMapStarted, MapRoster: &roster},
		Finished: journal.Event{Type: journal.EventMapFinished},
	}
	for i := range roster.Items {
		item := roster.Items[i]
		plan.Branches = append(plan.Branches, fanOutBranch{
			ID: i + 1, Name: item.Identity, Start: def.Task,
			ItemIdentity: item.Identity, ItemDigest: item.Digest,
			Bind: staticMapBinding(def.Bindings, item, roster.Source.Integrity),
		})
	}
	return plan, nil
}

func staticMapBinding(
	bindings map[string]apiv1.MapBinding,
	item journal.MapItem,
	integrity apiv1.Integrity,
) func(fanOutBindingContext) (fanOutBindings, error) {
	return func(fanOutBindingContext) (fanOutBindings, error) {
		values := make(map[string]any, len(bindings))
		grades := make(map[string]apiv1.Integrity, len(bindings))
		exact := make(map[string]json.RawMessage)
		for name, binding := range bindings {
			switch binding {
			case apiv1.MapBindingItem:
				decoder := json.NewDecoder(bytes.NewReader(item.Item))
				decoder.UseNumber()
				var value map[string]any
				if err := decoder.Decode(&value); err != nil {
					return fanOutBindings{}, fmt.Errorf(
						"decode frozen map item %q: %w", item.Identity, err,
					)
				}
				values[name] = value
				exact[name] = append(json.RawMessage(nil), item.Item...)
			case apiv1.MapBindingIdentity:
				values[name] = item.Identity
			case apiv1.MapBindingDigest:
				values[name] = item.Digest
			default:
				return fanOutBindings{}, fmt.Errorf(
					"map binding %q has unknown value %q", name, binding,
				)
			}
			grades[name] = integrity
		}
		return fanOutBindings{Values: values, Grades: grades, Exact: exact}, nil
	}
}

func assembleStaticMapJoin(
	def apiv1.Map,
	roster *journal.MapRoster,
	result fanOutResult,
) *staticMapJoin {
	join := &staticMapJoin{
		completeness:    make([]journal.BranchOutcome, len(result.Settlements)),
		results:         make([]runner.MapItemResult, 0, len(result.Settlements)),
		rosterIntegrity: roster.Source.Integrity,
	}
	grades := []apiv1.Integrity{join.rosterIntegrity}
	for i, settlement := range result.Settlements {
		outcome := settlement.Outcome
		join.completeness[i] = outcome
		entry := runner.MapItemResult{
			Identity: outcome.MapItemIdentity, Digest: outcome.MapItemDigest,
			Branch: outcome.Branch, Status: outcome.Status,
		}
		if mapped, ok := settlement.Results[def.Task]; ok {
			entry.Outputs, entry.Integrity = mapped.Outputs, mapped.Integrity
			entry.Pointers = append([]apiv1.ContextPointer(nil), settlement.Pointers...)
			for j := range entry.Pointers {
				entry.Pointers[j].Branch, entry.Pointers[j].BranchName = outcome.Branch, outcome.Name
			}
			grades = append(grades, mapped.Integrity)
		}
		join.results = append(join.results, entry)
	}
	join.resultsIntegrity = apiv1.WeakestIntegrity(grades...)
	return join
}
