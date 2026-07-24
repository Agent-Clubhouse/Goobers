package workerhost

import (
	"context"
	"fmt"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// WorktreeWorkspaces is the production engine.WorkspaceProvisioner: a fresh,
// isolated, disposable working copy per stage attempt, provisioned exactly as
// the local runner's createStageWorkspace does — a git worktree on the run
// branch for repo mode, an empty temp directory for scratch mode. Without a
// wired provisioner the engine fails every workspace-needing stage closed
// (#621), so this is what makes `goobers worker` able to execute real stages.
type WorktreeWorkspaces struct {
	// Manager provisions repo-mode worktrees (mirror clone + git worktree add).
	Manager *worktree.Manager
	// ScratchDir roots scratch-mode workspaces.
	ScratchDir string
	// CloneURL derives the git remote for a RepoRef. Nil applies the local
	// runner's own derivation (runner.DefaultRepoCloneURL).
	CloneURL func(apiv1.RepoRef) (string, error)
}

// Provision implements engine.WorkspaceProvisioner.
func (p *WorktreeWorkspaces) Provision(ctx context.Context, req engine.WorkspaceRequest) (engine.Workspace, error) {
	switch req.Mode {
	case apiv1.WorkspaceScratch:
		if req.SyncBase {
			// Compilation rejects the combination (v_current checks); this
			// guards a request constructed without it, same as the local
			// runner's createStageWorkspace.
			return nil, fmt.Errorf("workerhost: scratch workspace for stage %q: syncBase requires a repo workspace", req.Stage)
		}
		if p.ScratchDir == "" {
			return nil, fmt.Errorf("workerhost: scratch workspace for stage %q: no scratch dir configured", req.Stage)
		}
		if err := os.MkdirAll(p.ScratchDir, 0o700); err != nil {
			return nil, fmt.Errorf("workerhost: create scratch root: %w", err)
		}
		path, err := os.MkdirTemp(p.ScratchDir, "goobers-scratch-*")
		if err != nil {
			return nil, fmt.Errorf("workerhost: create scratch workspace: %w", err)
		}
		return scratchWorkspace(path), nil
	case "", apiv1.WorkspaceRepo:
		if p.Manager == nil {
			return nil, fmt.Errorf("workerhost: repo workspace for stage %q: no worktree manager configured", req.Stage)
		}
		cloneURL := p.CloneURL
		if cloneURL == nil {
			cloneURL = runner.DefaultRepoCloneURL
		}
		repoURL, err := cloneURL(req.RepoRef)
		if err != nil {
			return nil, err
		}
		baseRef := req.RepoRef.Branch
		if baseRef == "" {
			baseRef = "main"
		}
		branch := providers.BranchNameIn(
			providers.NormalizeBranchNamespace(req.BranchNamespace),
			req.Workflow, req.RunID,
		)
		wt, err := p.Manager.Create(ctx, worktree.CreateOptions{
			RepoURL:    repoURL,
			RunID:      req.RunID + "-" + req.Stage,
			OwnerRunID: req.RunID,
			BaseRef:    baseRef,
			Branch:     branch,
			SyncBase:   req.SyncBase,
		})
		if err != nil {
			return nil, fmt.Errorf("workerhost: create worktree for stage %q: %w", req.Stage, err)
		}
		return &worktreeWorkspace{wt: wt}, nil
	default:
		return nil, fmt.Errorf("workerhost: unknown workspace mode %q for stage %q", req.Mode, req.Stage)
	}
}

type scratchWorkspace string

func (w scratchWorkspace) Path() string { return string(w) }

func (w scratchWorkspace) Remove(context.Context) error { return os.RemoveAll(string(w)) }

type worktreeWorkspace struct {
	wt *worktree.Worktree
}

func (w *worktreeWorkspace) Path() string { return w.wt.Path }

func (w *worktreeWorkspace) Remove(ctx context.Context) error {
	return w.wt.Remove(ctx, worktree.RemoveOptions{})
}
