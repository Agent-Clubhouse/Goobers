package worktree

import (
	"context"
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
		return wt.syncStageBase(ctx, repository, base)
	})
	if err == nil && !found {
		return fmt.Errorf("worktree: held parent mirror unavailable")
	}
	return err
}
