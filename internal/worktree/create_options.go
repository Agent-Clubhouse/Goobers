package worktree

import (
	"context"
	"fmt"
)

func normalizeCreateOptions(opts *CreateOptions) error {
	if err := validateOwnedStartingSHA(opts.OwnedStartingSHA, opts.Branch, opts.SyncBase); err != nil {
		return err
	}
	if opts.ExpectedSHA != "" {
		if err := validateRevisionOptions(*opts); err != nil {
			return err
		}
	}
	if opts.RunID == "" {
		return fmt.Errorf("worktree: RunID is required")
	}
	if !validRunID(opts.RunID) {
		return fmt.Errorf("worktree: RunID %q must be a single path segment (no \"..\", no \"/\")", opts.RunID)
	}
	if opts.OwnerRunID == "" {
		opts.OwnerRunID = opts.RunID
	}
	if !validRunID(opts.OwnerRunID) {
		return fmt.Errorf("worktree: OwnerRunID %q must be a single path segment (no \"..\", no \"/\")", opts.OwnerRunID)
	}
	if opts.BaseRef == "" {
		return fmt.Errorf("worktree: BaseRef is required")
	}
	if opts.SyncBase && opts.Branch == "" {
		return fmt.Errorf("worktree: SyncBase requires Branch")
	}
	if opts.AcquireRemoteBranch && !opts.RequireExistingBranch {
		return fmt.Errorf("worktree: AcquireRemoteBranch requires RequireExistingBranch")
	}
	return nil
}

func (m *Manager) materializeSparseWorktree(ctx context.Context, path, target string, opts CreateOptions, partial bool) error {
	runCheckout := func(args ...string) error {
		if opts.ExpectedSHA != "" || opts.OwnedStartingSHA != "" {
			return m.runRevisionGit(ctx, opts.RepoURL, path, args...)
		}
		return runGit(ctx, path, args...)
	}
	setArgs := append([]string{"sparse-checkout", "set", "--cone"}, opts.Sparse...)
	if err := runCheckout(setArgs...); err != nil {
		return fmt.Errorf("worktree: configure sparse checkout for run %s: %w", opts.RunID, err)
	}
	// The worktree was added without checkout: only declared cones and root
	// files are materialized, including when the mirror is blobless.
	checkoutArgs := []string{"checkout", target}
	if opts.ExpectedSHA != "" {
		return runCheckout("checkout", "--detach", target)
	}
	if opts.OwnedStartingSHA != "" {
		return runCheckout(checkoutArgs...)
	}
	var err error
	if partial {
		err = m.runRemoteGit(ctx, opts.RepoURL, path, checkoutArgs...)
	} else {
		err = runGit(ctx, path, checkoutArgs...)
	}
	if err != nil {
		return fmt.Errorf("worktree: materialize sparse checkout for run %s: %w", opts.RunID, err)
	}
	return nil
}
