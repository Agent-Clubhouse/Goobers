package recovery

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// AdoptRestoredCommit fast-forwards an exclusively owned receiving worktree's
// current branch to a previously verified restored commit. The caller must
// bind branch and expectedHead to that receiving run, not to request input.
// Unlike operator restoration, this explicitly changes the checkout and index.
// It never resets, stashes, switches branches, or overwrites ignored files.
// A retry already at restored is acknowledged; a different HEAD is refused.
func AdoptRestoredCommit(ctx context.Context, repository, branch, expectedHead, restored string) error {
	if !gitObjectID.MatchString(expectedHead) || !gitObjectID.MatchString(restored) {
		return fmt.Errorf("recovery adoption requires exact commit identities")
	}
	ref := "refs/heads/" + branch
	if err := recoveryGit(ctx, repository, io.Discard, "check-ref-format", ref); err != nil {
		return fmt.Errorf("invalid receiving branch")
	}
	var symbolic snapshotPathOutput
	if err := recoveryGit(ctx, repository, &symbolic, "symbolic-ref", "--quiet", "HEAD"); err != nil || strings.TrimSpace(symbolic.String()) != ref {
		return fmt.Errorf("recovery adoption requires the receiving run's checked-out branch")
	}
	var head boundedRefOutput
	if err := recoveryGit(ctx, repository, &head, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return err
	}
	current := strings.TrimSpace(head.String())
	if current != expectedHead && current != restored {
		return fmt.Errorf("receiving branch changed before recovery adoption")
	}
	// Refuse any tracked/index/untracked work, not only paths touched by the
	// incoming commit. Bound output without buffering a large dirty checkout.
	var status boundedRefOutput
	if err := recoveryGit(ctx, repository, &status, "status", "--porcelain=v1", "--untracked-files=all"); err != nil {
		return fmt.Errorf("cannot verify clean receiving worktree: %w", err)
	}
	if status.Len() != 0 {
		return fmt.Errorf("recovery adoption requires a clean receiving worktree")
	}
	if current == restored {
		return nil
	}
	if err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", expectedHead, restored); err != nil {
		return fmt.Errorf("restored state does not fast-forward the receiving branch")
	}
	if err := recoveryGit(ctx, repository, io.Discard,
		"-c", "merge.autoStash=false", "merge", "--ff-only", "--no-autostash", "--no-overwrite-ignore", "--no-edit", "--no-stat", restored); err != nil {
		return fmt.Errorf("adopt restored state without discarding local work: %w", err)
	}
	return nil
}
