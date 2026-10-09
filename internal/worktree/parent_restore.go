package worktree

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ParentRestoreOptions identifies a fresh physical workspace for a retained
// parent. The caller verifies/imports the archive, durably records restoration
// intent and owns the run lease. BaseRef remains the cumulative source base;
// HeadSHA is the exact archived commit, not a synthetic recovery snapshot.
type ParentRestoreOptions struct {
	RepoURL, RunID, OwnerRunID, Gaggle string
	Branch, BaseRef, HeadSHA, StartRef string
}

// CreateParentRestore provisions from an already imported archive in the
// managed mirror without refreshing source refs. Partial-mirror checkout uses
// the normal credential gate. Existing workspaces and branches are never reset.
// An absent branch may be recreated at the explicit archived HEAD. A present
// branch must match it exactly; identical retries preserve partial restoration.
func (m *Manager) CreateParentRestore(ctx context.Context, opts ParentRestoreOptions) (*Worktree, error) {
	if err := validateParentRestoreOptions(opts); err != nil {
		return nil, err
	}
	var repository string
	found, err := m.WithExistingMirror(ctx, opts.RepoURL, func(dir string) error {
		repository = dir
		return runGit(ctx, dir, "merge-base", "--is-ancestor", opts.StartRef, opts.HeadSHA)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("parent archive has not been imported into the managed mirror")
	}
	create := CreateOptions{RepoURL: opts.RepoURL, RunID: opts.RunID, OwnerRunID: opts.OwnerRunID, Gaggle: opts.Gaggle, Branch: opts.Branch, BaseRef: opts.BaseRef, RequireExistingBranch: true, RetainOnCleanup: true, parentRestoreHead: opts.HeadSHA, parentRestoreStart: opts.StartRef}
	return m.createInMirror(ctx, create, repository, false)
}

func validateParentRestoreOptions(opts ParentRestoreOptions) error {
	if opts.RepoURL == "" || !validRunID(opts.RunID) || !validRunID(opts.OwnerRunID) || opts.Gaggle == "" || opts.Branch == "" || opts.BaseRef == "" || strings.HasPrefix(opts.BaseRef, "-") || strings.ContainsAny(opts.BaseRef, "\x00\r\n") {
		return fmt.Errorf("parent restore requires exact workspace ownership and source")
	}
	for _, ref := range []string{opts.HeadSHA, opts.StartRef} {
		if _, err := hex.DecodeString(ref); err != nil || ref != strings.ToLower(ref) || (len(ref) != 40 && len(ref) != 64) {
			return fmt.Errorf("parent restore requires exact archived HEAD and custody anchor")
		}
	}
	return nil
}

func (m *Manager) existingPreservedWorkspace(ctx context.Context, key, repository, path string, opts CreateOptions, child bool) (*Worktree, bool, error) {
	if opts.parentRestoreHead != "" {
		return m.existingParentRestore(ctx, key, repository, path, opts)
	}
	if child {
		return m.existingChildWorktree(ctx, key, repository, path, opts)
	}
	return nil, false, nil
}

func (m *Manager) existingParentRestore(ctx context.Context, key, repository, path string, opts CreateOptions) (*Worktree, bool, error) {
	if err := runGit(ctx, repository, "check-ref-format", "refs/heads/"+opts.Branch); err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		for _, markerPath := range []string{m.markerPath(key, opts.RunID), m.ownershipPath(key, worktreeDirectoryName(opts.RunID))} {
			if _, err := os.Lstat(markerPath); !errors.Is(err, os.ErrNotExist) {
				return nil, false, fmt.Errorf("parent restore has incomplete workspace ownership")
			}
		}
		if err := ensureParentRestoreBranch(ctx, repository, opts); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	if err != nil || !info.IsDir() {
		return nil, false, fmt.Errorf("parent restore target is not a managed directory")
	}
	wt, err := m.adoptParentRestore(ctx, key, repository, path, opts)
	return wt, err == nil, err
}

func ensureParentRestoreBranch(ctx context.Context, repository string, opts CreateOptions) error {
	ref := "refs/heads/" + opts.Branch
	if err := runGit(ctx, repository, "symbolic-ref", "--quiet", ref); err == nil {
		return fmt.Errorf("parent restore refuses a symbolic branch alias")
	} else {
		var commandErr *gitCommandError
		if !errors.As(err, &commandErr) || commandErr.exitCode != 1 || strings.TrimSpace(string(commandErr.output)) != "" {
			return err
		}
	}
	if branchExists(ctx, repository, opts.Branch) {
		sha, err := gitOutput(ctx, repository, "rev-parse", "--verify", ref)
		if err != nil || sha != opts.parentRestoreHead {
			return fmt.Errorf("parent restore branch differs from archived HEAD")
		}
		return nil
	}
	return runGit(ctx, repository, "update-ref", "--no-deref", ref, opts.parentRestoreHead, strings.Repeat("0", len(opts.parentRestoreHead)))
}

func (m *Manager) adoptParentRestore(ctx context.Context, key, repository, path string, opts CreateOptions) (*Worktree, error) {
	wt := &Worktree{RunID: opts.RunID, Path: path, Branch: opts.Branch, manager: m, key: key, startRef: opts.parentRestoreStart, repoURL: opts.RepoURL}
	mk, ownership, err := wt.parentRestoreMarkers(opts)
	if err != nil {
		return nil, err
	}
	if mk.OwnerRunID != opts.OwnerRunID || mk.Gaggle != opts.Gaggle || mk.StartRef != opts.parentRestoreStart || mk.BaseRef != resolvedCleanupBaseRef(ctx, repository, opts.BaseRef) || !mk.RetainOnCleanup {
		return nil, fmt.Errorf("parent restore workspace identity changed")
	}
	registered, err := worktreeRegistered(ctx, repository, path)
	if err != nil || !registered {
		return nil, fmt.Errorf("parent restore workspace registration is unavailable")
	}
	branch, err := gitOutput(ctx, path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || branch != "refs/heads/"+opts.Branch {
		return nil, fmt.Errorf("parent restore workspace branch changed")
	}
	if err := ensureParentRestoreBranch(ctx, repository, opts); err != nil {
		return nil, err
	}
	// Archive authority and exact branch ownership supersede an interrupted
	// cleanup transition. Reestablish the hold before returning the checkout;
	// never reset its partially restored index or working files.
	mk.Status, ownership.Status = statusCleanupRetained, statusCleanupRetained
	mk.CleanupDisposition, ownership.CleanupDisposition = childWaitDisposition, childWaitDisposition
	if err := wt.writeCustodyMarkers(mk, ownership); err != nil {
		return nil, err
	}
	wt.assetGuard = mk.AssetPathGuard
	wt.partialMirror = m.partialClone && mirrorIsPartial(ctx, repository)
	return wt, nil
}

func (wt *Worktree) parentRestoreMarkers(opts CreateOptions) (marker, marker, error) {
	primary, err := readMarker(wt.manager.markerPath(wt.key, wt.RunID))
	if err != nil {
		return marker{}, marker{}, err
	}
	ownership, err := readMarker(wt.manager.ownershipPath(wt.key, filepath.Base(wt.Path)))
	if err != nil {
		return marker{}, marker{}, err
	}
	custody := StageCustody{WorkspaceID: wt.RunID, OwnerRunID: opts.OwnerRunID, RepositoryDigest: RepositoryDigest(opts.RepoURL), Branch: opts.Branch, StartRef: opts.parentRestoreStart}
	if err := archivedStageMarkers(primary, ownership, custody, filepath.Base(wt.Path)); err != nil {
		return marker{}, marker{}, err
	}
	return primary, ownership, nil
}

func initialWorktreeRef(ctx context.Context, path string, opts CreateOptions) (string, error) {
	head, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil || opts.parentRestoreHead == "" {
		return head, err
	}
	if head != opts.parentRestoreHead {
		return "", fmt.Errorf("parent restore checkout moved from archived HEAD")
	}
	return opts.parentRestoreStart, nil
}
