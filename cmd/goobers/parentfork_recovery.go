package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// Complete only recorded host reservations, without invoking agents. The caller
// owns the terminal journal, has checked child writers, and will archive each
// acquired checkout before any hold release or removal can occur.
func (r parentArchiveRestorer) recoverForkPlans(ctx context.Context, run *journal.Run, reader *journal.Reader, pending []runner.ParentForkRecovery) error {
	for _, item := range pending {
		if err := r.recoverForkPlan(ctx, run, reader, item); err != nil {
			return err
		}
	}
	return nil
}

func (r parentArchiveRestorer) recoverForkPlan(ctx context.Context, run *journal.Run, reader *journal.Reader, item runner.ParentForkRecovery) error {
	snapshot, err := parallelworkspace.ReadSource(reader, item.Plan.Source, item.Plan.Parallel, item.Plan.Sequence)
	if err != nil {
		return err
	}
	project, err := recoveryConfiguredProject(r.config, snapshot.Record.RepositoryKey)
	if err != nil {
		return err
	}
	url, err := r.cloneURL(project)
	if err != nil {
		return err
	}
	// Validate every reservation against the configured repository before source
	// import or partial creation. Changed endpoint/ownership cannot be retargeted.
	for index, owner := range item.Plan.Workspaces {
		expected, err := worktree.ParallelForkCustody(worktree.ParallelForkOptions{RepoURL: url, OwnerRunID: item.Plan.RunID, Gaggle: item.Plan.Gaggle, ParallelSequence: item.Plan.Sequence, Branch: index + 1, SnapshotSHA: item.Plan.Source.SnapshotSHA})
		if err != nil {
			return err
		}
		if owner != expected {
			return errors.New("terminal fork reservation changed repository or physical owner")
		}
	}
	backend := parallelworkspace.Service{Worktrees: r.worktrees, CloneURL: r.cloneURL}
	request := spec.Request{RunID: item.Plan.RunID, Gaggle: item.Plan.Gaggle, Parallel: item.Plan.Parallel, Sequence: item.Plan.Sequence, At: snapshot.Record.CreatedAt, Repository: project}
	source, err := backend.Prepare(ctx, run, request, &item.Plan.Source)
	if err != nil {
		return err
	}
	if source != item.Plan.Source {
		return errors.New("terminal fork source changed on recovery")
	}
	for _, branch := range item.Pending {
		if err := r.recoverForkCheckout(ctx, run, url, item, branch); err != nil {
			return err
		}
	}
	return nil
}

func (r parentArchiveRestorer) recoverForkCheckout(ctx context.Context, run *journal.Run, url string, item runner.ParentForkRecovery, branch int) error {
	if branch <= 0 || branch > len(item.Plan.Workspaces) {
		return errors.New("terminal fork branch outside reservation")
	}
	owner := item.Plan.Workspaces[branch-1]
	_, err := r.worktrees.AdoptHeldStage(ctx, url, owner)
	if err != nil {
		checkout, err := r.worktrees.CreateParallelFromSnapshot(ctx, worktree.ParallelForkOptions{RepoURL: url, OwnerRunID: item.Plan.RunID, Gaggle: item.Plan.Gaggle, ParallelSequence: item.Plan.Sequence, Branch: branch, SnapshotSHA: item.Plan.Source.SnapshotSHA})
		if err != nil {
			return err
		}
		held, err := checkout.HoldForChild(ctx)
		if err != nil {
			return err
		}
		if held != owner {
			return errors.New("terminal fork recovered unexpected custody")
		}
	}
	return runner.RecordParentForkReady(run, item.Plan.Sequence, item.Reference, branch, owner)
}
