package worktree

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func (wt *Worktree) preparePinnedOwnedBranch(ctx context.Context, opts PinnedPrepareOptions, baseRef, localTip string) error {
	if err := apiv1.ValidateCommitSHA(opts.OwnedStartingSHA); err != nil {
		return revisionFailure(workspacerevision.CodeInvalid, "invalid owned pinned starting SHA", err)
	}
	remote := "refs/remotes/mirror/" + opts.Branch
	if _, err := gitOutput(ctx, wt.Path, "--no-replace-objects", "--no-lazy-fetch", "merge-base", "--is-ancestor", opts.OwnedStartingSHA, remote); err != nil {
		return revisionFailure(workspacerevision.CodeConflict, "owned remote branch lost its starting SHA", err)
	}
	if _, err := gitOutput(ctx, wt.Path, "--no-replace-objects", "--no-lazy-fetch", "merge-base", "--is-ancestor", opts.OwnedStartingSHA, localTip); err == nil {
		// Preserve unpushed local work; a remote advance is reconciled by the
		// publication broker's expected-tip check, never by implicit rebasing.
		return nil
	}
	baseTip, err := gitOutput(ctx, wt.Path, "rev-parse", "--verify", baseRef+"^{commit}")
	if err != nil || baseTip != localTip {
		return revisionFailure(workspacerevision.CodeConflict, "pinned branch has pre-establishment work; refusing to discard it", err)
	}
	// AcquirePinned creates this empty base placeholder before the scratch
	// selector runs. Only that unchanged placeholder can adopt the remote root.
	if err := wt.manager.runRevisionGit(ctx, wt.repoURL, wt.Path, "reset", "--hard", remote); err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "adopt owned pinned branch", err)
	}
	return nil
}
