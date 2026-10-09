package runner

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

type parallelRuntime struct {
	capacity *parallelChildCapacity
	forks    []worktree.StageCustody
}

func (r *Runner) prepareParallelRuntime(ctx context.Context, run *journal.Run, in StartInput, par *parallelExec, branch string, events []journal.Event) (parallelRuntime, error) {
	capacity, err := r.parallelChildOwner(in, par, events)
	if err != nil {
		return parallelRuntime{}, err
	}
	if parallelHasChildStage(in.Machine, par.spec) && r.cfg.PrepareParentForkResult == nil {
		return parallelRuntime{}, errors.New("parallel fork result service unavailable before dispatch")
	}
	forks, err := r.prepareParallelForks(ctx, run, in, par.spec, branch, events)
	return parallelRuntime{capacity: capacity, forks: forks}, err
}

func parallelBranchInput(in StartInput, runtime parallelRuntime, slot *parallelBranchSlot, branch int, workspaceBranch string) StartInput {
	in.parallelSlot, in.parallelChild = slot, runtime.capacity
	in.WorkspaceBranch = workspaceBranch
	if len(runtime.forks) != 0 {
		custody := runtime.forks[branch-1]
		in.parallelWorkspace = &custody
		in.WorkspaceBranch, in.WorkspaceBranchSHA = custody.Branch, ""
	}
	return in
}

func (r *Runner) parallelStageWorkspace(ctx context.Context, in StartInput, mode apiv1.WorkspaceMode, syncBase bool, branch string) (*stageWorkspace, error) {
	if in.parallelWorkspace == nil || mode == apiv1.WorkspaceScratch || mode == apiv1.WorkspaceRepoReadOnly {
		return nil, nil
	}
	if mode != "" && mode != apiv1.WorkspaceRepo {
		return nil, errors.New("invalid parallel workspace mode")
	}
	owner := *in.parallelWorkspace
	if in.Child != nil || in.pinnedWorkspace != nil || r.cfg.Worktrees == nil || r.cfg.RepoCloneURL == nil || owner.OwnerRunID != in.RunID || branch != owner.Branch {
		return nil, errors.New("parallel stage workspace differs from its reserved fork")
	}
	if err := selectedWorkspaceUnsupported(in, mode); err != nil {
		return nil, err
	}
	url, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return nil, err
	}
	workspace, err := r.cfg.Worktrees.AdoptHeldStage(ctx, url, owner)
	if err != nil {
		return nil, err
	}
	base := in.RepoRef.Branch
	if base == "" {
		base = "main"
	}
	if err := workspace.PrepareHeldStage(ctx, base, syncBase); err != nil {
		return nil, err
	}
	return &stageWorkspace{path: workspace.Path, worktree: workspace, parentContribution: true, retainedChild: func(context.Context) error { return nil }}, nil
}

// Resolve existing execution ownership before any ordinary stage provisioning.
// The ordering preserves exact child recovery custody, then branch fork custody,
// then serial parent continuity and launcher-provisioned child workspaces.
func (r *Runner) ownedStageWorkspace(ctx context.Context, in StartInput, stageName string, mode apiv1.WorkspaceMode, syncBase bool, workspaceBranch string) (*stageWorkspace, error) {
	if in.heldChildWorkspace != nil {
		return in.heldChildWorkspace, nil
	}
	if fork, err := r.parallelStageWorkspace(ctx, in, mode, syncBase, workspaceBranch); fork != nil || err != nil {
		return fork, err
	}
	if inherited, err := r.inheritedParentWorkspace(ctx, in, mode, syncBase, workspaceBranch); inherited != nil || err != nil {
		return inherited, err
	}
	if in.ChildWorkspace != nil && mode != apiv1.WorkspaceScratch {
		return r.createChildStageWorkspace(ctx, in, mode, syncBase, workspaceBranch)
	}

	return nil, nil
}
