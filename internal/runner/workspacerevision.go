package runner

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/internal/worktree"
)

func (r *Runner) acceptWorkspaceRevision(in StartInput, task apiv1.Task, result apiv1.ResultEnvelope) (*apiv1.WorkspaceRevision, error) {
	return acceptWorkspaceRevision(in, task, result, r.cfg.AdditionalRepos)
}

func acceptWorkspaceRevision(in StartInput, task apiv1.Task, result apiv1.ResultEnvelope, additional []apiv1.RepoRef) (*apiv1.WorkspaceRevision, error) {
	selected, err := workspacerevision.Accept(in.workspaceRevision, result.WorkspaceRevision,
		task.Type == apiv1.TaskDeterministic, result.Status == apiv1.ResultSuccess)
	if err != nil {
		return nil, err
	}
	if selected != nil {
		if _, err := workspacerevision.Resolve(*selected, in.RepoRef, additional); err != nil {
			return nil, err
		}
	}
	return selected, nil
}

func (r *Runner) restoreWorkspaceRevision(events []journal.Event, in StartInput) (*apiv1.WorkspaceRevision, error) {
	return RestoredWorkspaceRevision(events, in.Machine, in.RepoRef, r.cfg.AdditionalRepos)
}

// RestoredWorkspaceRevision reconstructs accepted authority from journal history,
// applying the same producer and configured-repository checks as the live runner.
func RestoredWorkspaceRevision(events []journal.Event, machine *workflow.Machine, base apiv1.RepoRef, additional []apiv1.RepoRef) (*apiv1.WorkspaceRevision, error) {
	in := StartInput{Machine: machine, RepoRef: base}
	for _, event := range events {
		if event.WorkspaceRevision == nil {
			continue
		}
		task, exists := in.Machine.Task(event.Stage)
		if event.Type != journal.EventStageFinished || !exists {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "workspace revision has no deterministic stage result"}
		}
		selected, err := acceptWorkspaceRevision(in, task, apiv1.ResultEnvelope{
			Status: apiv1.ResultStatus(event.Status), WorkspaceRevision: event.WorkspaceRevision,
		}, additional)
		if err != nil {
			return nil, fmt.Errorf("restore workspace revision at event %d: %w", event.Seq, err)
		}
		in.workspaceRevision = selected
	}
	return in.workspaceRevision, nil
}

func (r *Runner) createRevisionWorkspace(ctx context.Context, in StartInput, stage string, syncBase bool, branch string) (*stageWorkspace, error) {
	if syncBase || branch != "" {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeConflict, Message: "selected-revision repo-readonly cannot use syncBase or workspaceBranch"}
	}
	selected := in.workspaceRevision
	repo, err := workspacerevision.Resolve(*selected, in.RepoRef, r.cfg.AdditionalRepos)
	if err != nil {
		return nil, err
	}
	repoURL, err := r.cfg.RepoCloneURL(repo)
	if err != nil {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "source repository access failed", Cause: err}
	}
	sparse := sparseCones(repo.Checkout)
	if in.pinnedWorkspace != nil {
		in.pinnedStage.Lock()
		if err := in.pinnedWorkspace.PreparePinnedRevision(ctx, repoURL, selected.CommitSHA, sparse); err != nil {
			in.pinnedStage.Unlock()
			return nil, err
		}
		additional, err := r.provisionAdditionalCheckouts(ctx, in, stage)
		if err != nil {
			resetErr := in.pinnedWorkspace.ResetPinnedRevision(ctx, selected.CommitSHA)
			in.pinnedStage.Unlock()
			return nil, errors.Join(err, resetErr)
		}
		return &stageWorkspace{
			path: in.pinnedWorkspace.Path, worktree: in.pinnedWorkspace,
			sparse: sparse, additional: additional, release: in.pinnedStage.Unlock,
			reset: func(ctx context.Context) error {
				return in.pinnedWorkspace.ResetPinnedRevision(ctx, selected.CommitSHA)
			},
		}, nil
	}
	wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
		RepoURL: repoURL, RunID: in.RunID + "-" + stage, OwnerRunID: in.RunID,
		BaseRef: selected.CommitSHA, ExpectedSHA: selected.CommitSHA, Sparse: sparse,
	})
	if err != nil {
		return nil, err
	}
	additional, err := r.provisionAdditionalCheckouts(ctx, in, stage)
	if err != nil {
		return nil, errors.Join(err, wt.Remove(ctx, worktree.RemoveOptions{}))
	}
	return &stageWorkspace{path: wt.Path, worktree: wt, sparse: sparse, additional: additional}, nil
}
