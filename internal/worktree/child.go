package worktree

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/workspacedelta"
)

// ChildOptions names an isolated workspace for an accepted child. The caller
// authorizes the repository/lineage and holds exclusive child execution custody.
// RunID is the stage workspace identity; OwnerRunID is the child workflow run.
type ChildOptions struct {
	RepoURL    string
	RunID      string
	OwnerRunID string
	Gaggle     string
	// SnapshotSHA is the verified immutable commit imported and pinned by the
	// recovery carrier. This low-level method never imports untrusted bytes.
	SnapshotSHA string
}

// CreateChildFromSnapshot provisions an isolated child branch from an already
// verified, pinned snapshot in the managed mirror. Callers use WithRecoveryMirror
// with recovery.ImportChildSnapshot first; keeping import outside this package
// preserves its dependency boundary. Mirror acquisition is the caller's job;
// an existing partial mirror hydrates checkout blobs through the normal manager
// credential and remote-operation gate. The parent checkout is not mutated.
// Identical retries preserve owned child edits; incomplete or mismatched custody
// refuses for coordinator recovery.
func (m *Manager) CreateChildFromSnapshot(ctx context.Context, opts ChildOptions) (*Worktree, error) {
	create, err := childCreateOptions(opts)
	if err != nil {
		return nil, err
	}
	var repository string
	found, err := m.WithExistingMirror(ctx, opts.RepoURL, func(dir string) error {
		repository = dir
		_, err := gitOutput(ctx, dir, "rev-parse", "--verify", opts.SnapshotSHA+"^{commit}")
		return err
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("child snapshot has not been imported into the managed mirror")
	}
	return m.createInMirror(ctx, create, repository, true)
}

func childCreateOptions(opts ChildOptions) (CreateOptions, error) {
	if opts.RepoURL == "" || !validRunID(opts.RunID) || !validRunID(opts.OwnerRunID) {
		return CreateOptions{}, fmt.Errorf("child workspace requires valid ownership and repository")
	}
	if _, err := hex.DecodeString(opts.SnapshotSHA); err != nil || opts.SnapshotSHA != strings.ToLower(opts.SnapshotSHA) || (len(opts.SnapshotSHA) != 40 && len(opts.SnapshotSHA) != 64) {
		return CreateOptions{}, fmt.Errorf("child workspace requires an exact snapshot commit")
	}
	return CreateOptions{RepoURL: opts.RepoURL, RunID: opts.RunID, OwnerRunID: opts.OwnerRunID, Gaggle: opts.Gaggle, BaseRef: opts.SnapshotSHA, Branch: "goobers/children/" + opts.OwnerRunID, RetainOnCleanup: true}, nil
}

// AdoptChildFromSnapshot verifies existing exact custody without provisioning,
// fetching, resetting, or removing anything. The caller holds the child run's
// execution lease; this repository lock only serializes manager operations.
// Missing or incomplete custody requires explicit recovery, never recreation.
func (m *Manager) AdoptChildFromSnapshot(ctx context.Context, opts ChildOptions) (*Worktree, error) {
	create, err := childCreateOptions(opts)
	if err != nil {
		return nil, err
	}
	var adopted *Worktree
	found, err := m.WithExistingMirror(ctx, opts.RepoURL, func(dir string) error {
		key := repoKey(opts.RepoURL)
		path := filepath.Join(m.runsDirForKey(key), worktreeDirectoryName(opts.RunID))
		var exists bool
		var err error
		adopted, exists, err = m.existingChildWorktree(ctx, key, dir, path, create)
		if err == nil && !exists {
			return fmt.Errorf("child workspace custody is missing; recovery is required")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("child workspace mirror is missing; recovery is required")
	}
	return adopted, nil
}

func (m *Manager) existingChildWorktree(ctx context.Context, key, repository, path string, opts CreateOptions) (*Worktree, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, markerErr := os.Lstat(m.markerPath(key, opts.RunID)); !errors.Is(markerErr, os.ErrNotExist) {
			return nil, false, fmt.Errorf("child workspace has incomplete custody; recovery is required")
		}
		if branchExists(ctx, repository, opts.Branch) {
			return nil, false, fmt.Errorf("child branch already exists without this workspace; recovery is required")
		}
		return nil, false, nil
	}
	if err != nil || !info.IsDir() {
		return nil, false, fmt.Errorf("child workspace is not a managed directory")
	}
	mk, err := readMarker(m.markerPath(key, opts.RunID))
	if err != nil {
		return nil, false, fmt.Errorf("child workspace custody is unavailable: %w", err)
	}
	ownership, err := readMarker(m.ownershipPath(key, worktreeDirectoryName(opts.RunID)))
	if err != nil {
		return nil, false, fmt.Errorf("child workspace ownership is unavailable: %w", err)
	}
	if !sameWorkspaceIdentity(mk, ownership) || mk.Status != statusActive || mk.RunID != opts.RunID || mk.OwnerRunID != opts.OwnerRunID || mk.Gaggle != opts.Gaggle || mk.BaseRef != opts.BaseRef || mk.Branch != opts.Branch || mk.StartRef != opts.BaseRef || mk.RepositoryDigest != RepositoryDigest(opts.RepoURL) {
		return nil, false, fmt.Errorf("child workspace custody conflicts with snapshot request")
	}
	registered, err := worktreeRegistered(ctx, repository, path)
	if err != nil || !registered {
		return nil, false, fmt.Errorf("child workspace registration is unavailable")
	}
	branch, err := gitOutput(ctx, path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || branch != "refs/heads/"+opts.Branch {
		return nil, false, fmt.Errorf("child workspace changed its owned branch")
	}
	ancestor, err := workspacedelta.IsAncestor(ctx, mirrorGit{}, path, opts.BaseRef, "HEAD")
	if err != nil || !ancestor {
		return nil, false, fmt.Errorf("child workspace no longer descends from its fork snapshot")
	}
	return &Worktree{RunID: opts.RunID, Path: path, Branch: opts.Branch, manager: m, key: key, startRef: mk.StartRef, repoURL: opts.RepoURL, partialMirror: m.partialClone && mirrorIsPartial(ctx, repository)}, true, nil
}
