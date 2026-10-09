package runner

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// prepareParallelForks runs before any branch goroutine or external writer.
// Recovery imports the recorded source; it never recaptures a newer root tree.
func (r *Runner) prepareParallelForks(ctx context.Context, run *journal.Run, in StartInput, parallel apiv1.Parallel, branch string, events []journal.Event) ([]worktree.StageCustody, error) {
	if !parallelHasChildStage(in.Machine, parallel) {
		return nil, nil
	}
	if in.Child != nil || in.pinnedWorkspace != nil || r.cfg.Worktrees == nil || r.cfg.RepoCloneURL == nil {
		return nil, errors.New("parallel parent forks require managed parent workspace ownership")
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return nil, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return nil, err
	}
	started, err := parallelForkBoundary(events, parallel)
	if err != nil {
		return nil, err
	}
	url, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return nil, err
	}
	state := states[started.Seq]
	if state == nil {
		plan, err := r.captureParallelForkSource(ctx, run, in, started, branch)
		if err != nil {
			return nil, err
		}
		owners, err := parallelForkOwners(url, in, plan, len(parallel.Branches))
		if err != nil {
			return nil, err
		}
		if err := reserveParentForks(events, states, owners); err != nil {
			return nil, err
		}
		plan.Workspaces = owners
		ref, err := recordParallelForkPlan(run, plan)
		if err != nil {
			return nil, err
		}
		state = &parentForkState{plan: plan, reference: ref, plannedAt: run.Seq(), ready: map[int]bool{}}
	}
	expected, err := parallelForkOwners(url, in, state.plan, len(parallel.Branches))
	if err != nil {
		return nil, err
	}
	if len(expected) != len(state.plan.Workspaces) {
		return nil, errors.New("parallel fork branch count changed")
	}
	for i := range expected {
		if expected[i] != state.plan.Workspaces[i] {
			return nil, errors.New("parallel fork identity changed on replay")
		}
	}
	if r.cfg.PrepareParentForkSource == nil {
		return nil, errors.New("parallel fork source service unavailable")
	}
	restored, err := r.cfg.PrepareParentForkSource(ctx, run, forkSourceRequest(in, started), &state.plan.Source)
	if err != nil {
		return nil, err
	}
	if restored != state.plan.Source {
		return nil, errors.New("parallel fork source changed on replay")
	}
	for index, owner := range state.plan.Workspaces {
		if err := r.prepareParallelFork(ctx, run, url, state, index+1, owner); err != nil {
			return nil, err
		}
	}
	return state.plan.Workspaces, nil
}

func parallelForkBoundary(events []journal.Event, parallel apiv1.Parallel) (journal.Event, error) {
	var started journal.Event
	for _, event := range events {
		if event.Type == journal.EventParallelStarted {
			started = event
		}
		if event.Type == journal.EventParallelFinished {
			started = journal.Event{}
		}
	}
	if started.Seq == 0 || started.Parallel != parallel.Name || started.Branch != 0 || len(started.Completeness) != len(parallel.Branches) || len(parallel.Branches) == 0 || len(parallel.Branches) > 128 {
		return started, errors.New("parallel fork lacks its current root boundary")
	}
	return started, nil
}

func parallelForkOwners(url string, in StartInput, plan ParentForkPlan, count int) ([]worktree.StageCustody, error) {
	owners := make([]worktree.StageCustody, 0, count)
	for branch := 1; branch <= count; branch++ {
		owner, err := worktree.ParallelForkCustody(worktree.ParallelForkOptions{RepoURL: url, OwnerRunID: in.RunID, Gaggle: in.Gaggle, ParallelSequence: plan.Sequence, Branch: branch, SnapshotSHA: plan.Source.SnapshotSHA})
		if err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, nil
}

func (r *Runner) prepareParallelFork(ctx context.Context, run *journal.Run, url string, state *parentForkState, branch int, owner worktree.StageCustody) error {
	_, err := r.cfg.Worktrees.AdoptHeldStage(ctx, url, owner)
	if err != nil {
		if state.ready[branch] {
			return fmt.Errorf("parallel ready workspace unavailable: %w", err)
		}
		workspace, createErr := r.cfg.Worktrees.CreateParallelFromSnapshot(ctx, worktree.ParallelForkOptions{RepoURL: url, OwnerRunID: owner.OwnerRunID, Gaggle: state.plan.Gaggle, ParallelSequence: state.plan.Sequence, Branch: branch, SnapshotSHA: owner.StartRef})
		if createErr != nil {
			return createErr
		}
		custody, holdErr := workspace.HoldForChild(ctx)
		if holdErr != nil {
			return holdErr
		}
		if custody != owner {
			return errors.New("parallel fork acquired unexpected workspace custody")
		}
	}
	if state.ready[branch] {
		return nil
	}
	return RecordParentForkReady(run, state.plan.Sequence, state.reference, branch, owner)
}
