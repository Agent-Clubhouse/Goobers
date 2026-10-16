package worktree

import (
	"context"
	"fmt"
	"os"
)

// RunBranchDiff returns the unified diff of refs/heads/<branch> against
// baseRef (`git diff baseRef...refs/heads/<branch>`) read straight from
// repoURL's managed mirror — byte-identical to the Diff a worktree checked
// out on that branch reports, without checking the branch out.
//
// It exists for a reviewer whose own workspace is not on the run branch (a
// detached read-only checkout, or a scratch directory) but which must still
// judge what the run committed (#5414). Every worktree of a repo shares the
// mirror's refs, so the run branch the implementer committed to is visible
// here even while no worktree holds it.
//
// A mirror that does not exist yet, or a run branch that was never created,
// carries no committed change: both report an empty diff, not an error.
func (m *Manager) RunBranchDiff(ctx context.Context, repoURL, baseRef, branch string) ([]byte, error) {
	if baseRef == "" || branch == "" {
		return nil, fmt.Errorf("worktree: RunBranchDiff requires a baseRef and a branch")
	}
	repoDir := m.repoDirForKey(repoKey(repoURL))
	if _, err := os.Stat(repoDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worktree: run branch diff for %q: %w", branch, err)
	}
	if !branchExists(ctx, repoDir, branch) {
		return nil, nil
	}
	args := evidenceDiffRangeArgs(baseRef, "refs/heads/"+branch)
	var out []byte
	var err error
	if m.partialClone && mirrorIsPartial(ctx, repoDir) {
		// As in Worktree.Diff: on a blobless mirror the diff may fetch blobs
		// from the promisor remote, which needs the credential environment.
		out, err = m.remoteGitOutput(ctx, repoURL, repoDir, args...)
	} else {
		out, err = rawGitOutput(ctx, repoDir, nil, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("worktree: git diff %s...%s: %w", baseRef, branch, err)
	}
	return out, nil
}
