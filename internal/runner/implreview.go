package runner

import (
	"context"
	"sync"

	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// reviewerDiff reads the patch an agentic gate's reviewer is handed as
// evidence (#301). A reviewer on the run branch reads it from its own
// worktree, exactly as before. An implementation-review gate whose reviewer
// was NOT given the run branch — a detached repo-readonly checkout at base,
// or a scratch directory with no checkout at all — reads the same
// base...<run branch> patch from the managed mirror its worktrees share
// (#5414); otherwise the reviewer silently judged the base branch. Returns
// nil when there is nothing to read.
func (r *Runner) reviewerDiff(ctx context.Context, in StartInput, gateName, workspaceBranch string, wt *worktree.Worktree) ([]byte, error) {
	baseRef := in.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	if wt != nil && (wt.Branch != "" || in.pinnedWorkspace != nil) {
		return wt.Diff(ctx, baseRef)
	}
	if wt == nil && runsemantics.ReviewsImplementation(in.Machine, gateName) {
		// A scratch reviewer in a run whose branch lives in its own worktree
		// (a pinned project workspace, or a child run's admitted fork) reads
		// it there, under the same lock its stages hold.
		if own, lock := onBranchRunWorktree(in); own != nil {
			lock.Lock()
			defer lock.Unlock()
			return own.Diff(ctx, baseRef)
		}
	}
	if !r.readsRunBranchFromMirror(in, gateName) {
		if wt == nil {
			return nil, nil
		}
		return wt.Diff(ctx, baseRef)
	}
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return nil, err
	}
	branch := workspaceBranch
	if branch == "" {
		branch = providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
	}
	return r.cfg.Worktrees.RunBranchDiff(ctx, repoURL, baseRef, branch)
}

// reviewerDiffObservesRunBranch reports whether reviewerDiff read the run
// branch rather than a detached base checkout, i.e. whether an empty result
// is evidence that the run committed nothing (#5334, #5414).
func (r *Runner) reviewerDiffObservesRunBranch(in StartInput, gateName string, wt *worktree.Worktree) bool {
	if wt != nil {
		if wt.Branch != "" {
			return true
		}
		if in.pinnedWorkspace != nil {
			return false
		}
	} else if runsemantics.ReviewsImplementation(in.Machine, gateName) {
		if own, _ := onBranchRunWorktree(in); own != nil {
			return true
		}
	}
	return r.readsRunBranchFromMirror(in, gateName)
}

// onBranchRunWorktree returns the run's own on-branch worktree when the run
// branch lives outside the shared mirror's per-stage worktrees, with the lock
// that serializes the run's stages on it.
func onBranchRunWorktree(in StartInput) (*worktree.Worktree, sync.Locker) {
	if in.pinnedWorkspace != nil && in.pinnedStage != nil {
		return in.pinnedWorkspace, in.pinnedStage
	}
	if in.childWorkspace != nil && in.childWorkspace.worktree != nil {
		return in.childWorkspace.worktree, &in.childWorkspace.stage
	}
	return nil, nil
}

// readsRunBranchFromMirror reports whether reviewerDiff may read the run
// branch from the shared mirror: only for an implementation-review gate, and
// only where the run branch lives in this runner's own managed mirror (not a
// pinned project workspace or a child run's separate fork, which reviewerDiff
// reads in place).
func (r *Runner) readsRunBranchFromMirror(in StartInput, gateName string) bool {
	if r.cfg.Worktrees == nil || r.cfg.RepoCloneURL == nil {
		return false
	}
	if in.pinnedWorkspace != nil || in.ChildWorkspace != nil || in.heldChildWorkspace != nil {
		return false
	}
	return runsemantics.ReviewsImplementation(in.Machine, gateName)
}
