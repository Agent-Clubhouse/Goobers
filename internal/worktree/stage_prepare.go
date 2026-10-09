package worktree

import (
	"context"
	"errors"
	"fmt"
)

// PrepareHeldStage begins another serialized stage on the same retained
// checkout. It never resets dirty files or staging. The immutable custody
// markers retain their original ancestor; only this stage's commit baseline
// changes, so earlier stages' commits cannot masquerade as this stage's work.
func (wt *Worktree) PrepareHeldStage(ctx context.Context, base string, syncBase bool) error {
	found, err := wt.manager.WithExistingMirror(ctx, wt.repoURL, func(repository string) error {
		primary, _, err := wt.custodyMarkers()
		if err != nil {
			return err
		}
		if primary.Status != statusCleanupRetained || primary.CleanupDisposition != childWaitDisposition {
			return fmt.Errorf("worktree: stage requires held parent custody")
		}
		branch, err := gitOutput(ctx, wt.Path, "symbolic-ref", "--quiet", "HEAD")
		if err != nil || branch != "refs/heads/"+wt.Branch {
			return fmt.Errorf("worktree: held stage branch changed")
		}
		head, err := gitOutput(ctx, wt.Path, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		wt.startRef = head
		if !syncBase {
			return nil
		}
		if base == "" {
			return fmt.Errorf("worktree: held base synchronization requires a base ref")
		}
		status, err := gitOutput(ctx, wt.Path, "status", "--porcelain", "--untracked-files=normal")
		if err != nil {
			return err
		}
		if status != "" {
			return fmt.Errorf("worktree: commit or resolve retained parent edits before synchronizing base")
		}
		if err := wt.manager.fetchMirror(ctx, wt.repoURL, repository, true); err != nil {
			return err
		}
		return wt.mergeHeldBase(ctx, base)
	})
	if err == nil && !found {
		return fmt.Errorf("worktree: held parent mirror unavailable")
	}
	return err
}

func (wt *Worktree) mergeHeldBase(ctx context.Context, base string) error {
	args := []string{"merge", "--ff", "--no-edit", base}
	var mergeErr error
	if wt.partialMirror {
		mergeErr = wt.manager.runRemoteGit(ctx, wt.repoURL, wt.Path, args...)
	} else {
		mergeErr = runGit(ctx, wt.Path, args...)
	}
	if mergeErr == nil {
		return nil
	}
	resolved, resolveErr := resolveBaseSyncConflict(ctx, wt.Path)
	if resolved {
		return nil
	}
	conflicting, inspectErr := mergeConflictFiles(ctx, wt.Path)
	cleanupErr := runGit(context.WithoutCancel(ctx), wt.Path, "merge", "--abort")
	return baseSyncFailure(CreateOptions{RunID: wt.RunID, BaseRef: base, Branch: wt.Branch}, errors.Join(mergeErr, resolveErr), conflicting, inspectErr, cleanupErr)
}
