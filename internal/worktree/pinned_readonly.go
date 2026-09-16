package worktree

import (
	"context"
	"path/filepath"

	"github.com/goobers/goobers/internal/workspacerevision"
)

// PreparePinnedInspection detaches from the current committed state before a
// legacy read-only stage. Its previous writable branch is never advanced by
// commits made during inspection. The caller holds the run lease and stage lock.
func (wt *Worktree) PreparePinnedInspection(ctx context.Context, sparse []string) (string, error) {
	if !wt.pinned || wt.manager == nil {
		return "", revisionFailure(workspacerevision.CodeInvalid, "inspection requires a pinned checkout", nil)
	}
	sha, err := gitOutput(ctx, wt.Path, "--no-replace-objects", "--no-lazy-fetch", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	root := filepath.Join(wt.manager.pinnedRoot, wt.key)
	owner, err := readPinnedCustody(root)
	if err != nil {
		return "", err
	}
	owner.SelectedRevisionSHA, owner.RevisionSparse = sha, append([]string(nil), sparse...)
	if err := writeMarker(filepath.Join(root, pinnedCustodyFile), owner); err != nil {
		return "", err
	}
	wt.revisionSparse = append([]string(nil), sparse...)
	if err := wt.ResetPinnedRevision(ctx, sha); err != nil {
		return "", err
	}
	return sha, nil
}
