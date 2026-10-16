package worktree

import (
	"context"
	"fmt"
)

// PrepareChildStage verifies the admitted branch before establishing this stage's
// baseline. An explicit sync merges the host-fetched source base into that same
// branch; it never resets retained files or delegates provider credentials.
func (wt *Worktree) PrepareChildStage(ctx context.Context, opts ChildOptions, base string, syncBase bool) error {
	create, err := childCreateOptions(opts)
	if err != nil {
		return err
	}
	if wt.key != repoKey(opts.RepoURL) || wt.Branch != create.Branch {
		return fmt.Errorf("worktree: child stage ownership changed")
	}
	found, err := wt.manager.WithExistingMirror(ctx, opts.RepoURL, func(repository string) error {
		verified, exists, err := wt.manager.existingChildWorktree(ctx, wt.key, repository, wt.Path, create)
		if err != nil {
			return err
		}
		if !exists || verified.Path != wt.Path {
			return fmt.Errorf("worktree: child stage custody missing")
		}
		head, err := gitOutput(ctx, wt.Path, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		wt.startRef = head
		if !syncBase {
			return nil
		}
		return wt.syncStageBase(ctx, repository, base)
	})
	if err == nil && !found {
		return fmt.Errorf("worktree: child stage mirror unavailable")
	}
	return err
}
