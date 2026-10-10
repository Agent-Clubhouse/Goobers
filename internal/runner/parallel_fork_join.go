package runner

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
)

// ParentForkJoinFunc is the host's durable application boundary, not an agent tool.
type ParentForkJoinFunc func(context.Context, OwnedJournalRecorder, spec.JoinRequest) error

func (r *Runner) joinParallelForks(ctx context.Context, run *journal.Run, in StartInput, parallel apiv1.Parallel, runtime parallelRuntime, outcomes []*parallelBranchResult, runJoin bool) error {
	if !runJoin || len(runtime.forks) == 0 {
		return nil
	}
	if r.cfg.JoinParentFork == nil {
		return errors.New("parallel root application service unavailable")
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return err
	}
	request, err := parallelForkJoinRequest(reader, in, parallel, outcomes)
	if err != nil {
		return err
	}
	for _, result := range request.Results {
		if result.Status == journal.BranchSucceeded {
			return r.cfg.JoinParentFork(ctx, run, request)
		}
	}
	return nil // Failed branch carriers remain available without applying their code.
}

func parallelForkJoinRequest(reader *journal.Reader, in StartInput, parallel apiv1.Parallel, outcomes []*parallelBranchResult) (spec.JoinRequest, error) {
	var empty spec.JoinRequest
	events, err := reader.Events()
	if err != nil {
		return empty, err
	}
	started, err := parallelForkBoundary(events, parallel)
	if err != nil {
		return empty, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return empty, err
	}
	state := states[started.Seq]
	if state == nil || state.plan.Root == nil || !state.ready[0] || len(outcomes) != len(state.plan.Workspaces) {
		return empty, errors.New("parallel join lacks exact root reservation and outcomes")
	}
	if err := validateJoinWriters(events, state.plan); err != nil {
		return empty, err
	}
	results, err := ParentForkResults(reader, state.plan, state.reference)
	if err != nil {
		return empty, err
	}
	if len(results) != len(outcomes) {
		return empty, errors.New("parallel join lacks immutable branch results")
	}
	request := spec.JoinRequest{Request: forkSourceRequest(in, started), Plan: state.reference, Seed: state.plan.Source, Root: *state.plan.Root, Results: make([]spec.JoinResult, len(results))}
	for _, result := range results {
		index := result.Branch - 1
		outcome := outcomes[index]
		if outcome == nil || outcome.index != index || outcome.status != result.Status || outcome.paused {
			return empty, errors.New("parallel join result differs from settled outcome")
		}
		request.Results[index] = spec.JoinResult{Branch: result.Branch, Status: result.Status, Custody: state.plan.Workspaces[index], Source: result.Source}
	}
	return request, nil
}

func validateJoinWriters(events []journal.Event, plan ParentForkPlan) error {
	projection, err := journal.ProjectChildWaits(events)
	if err != nil {
		return err
	}
	if len(projection.Waits) != 0 {
		return errors.New("parallel join has unresolved child writers")
	}
	for _, owner := range plan.AllWorkspaces() {
		state, err := readParentWorkspaceState(events, plan.RunID, owner.Branch)
		if err != nil {
			return err
		}
		if state.hold.Workspace != owner || state.returnedAt <= state.heldAt || state.retiredAt != 0 {
			return errors.New("parallel join lacks returned physical workspace custody")
		}
	}
	return nil
}
