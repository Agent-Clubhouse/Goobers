package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ReleaseArchivedStage surrenders an exact parent checkout to normal cleanup.
// verify must validate its durable retirement and current contents; it runs
// under the repository lock and must not reacquire that lock. Marker repair is
// limited to interrupted hold/release/cleanup transitions of the same owner.
func (m *Manager) ReleaseArchivedStage(ctx context.Context, repoURL string, custody StageCustody, verify func(context.Context, CleanupTarget) error) error {
	if verify == nil || !validRunID(custody.WorkspaceID) || !validRunID(custody.OwnerRunID) || custody.RepositoryDigest != RepositoryDigest(repoURL) || custody.Branch == "" || custody.StartRef == "" {
		return errors.New("worktree: invalid archived stage release")
	}
	key := repoKey(repoURL)
	wt := &Worktree{RunID: custody.WorkspaceID, Path: filepath.Join(m.runsDirForKey(key), worktreeDirectoryName(custody.WorkspaceID)), Branch: custody.Branch, manager: m, key: key, repoURL: repoURL}
	found, err := m.WithExistingMirror(ctx, repoURL, func(string) error {
		return wt.releaseArchivedStage(ctx, custody, verify)
	})
	if err == nil && !found {
		err = errors.New("worktree: archived stage mirror unavailable")
	}
	return err
}

func (wt *Worktree) releaseArchivedStage(ctx context.Context, custody StageCustody, verify func(context.Context, CleanupTarget) error) error {
	m, key := wt.manager, wt.key
	primary, primaryErr := readMarker(m.markerPath(key, wt.RunID))
	ownership, ownershipErr := readMarker(m.ownershipPath(key, filepath.Base(wt.Path)))
	if errors.Is(primaryErr, os.ErrNotExist) && errors.Is(ownershipErr, os.ErrNotExist) {
		if _, err := os.Lstat(wt.Path); errors.Is(err, os.ErrNotExist) {
			return nil // A prior verified cleanup already removed both records.
		}
	}
	if primaryErr != nil || ownershipErr != nil {
		return errors.Join(primaryErr, ownershipErr)
	}
	if err := archivedStageMarkers(primary, ownership, custody, filepath.Base(wt.Path)); err != nil {
		return err
	}
	target := CleanupTarget{Path: wt.Path, WorktreeID: wt.RunID, OwnerRunID: primary.OwnerRunID, Gaggle: primary.Gaggle, BaseRef: primary.BaseRef, StartRef: primary.StartRef, RetainOnCleanup: primary.RetainOnCleanup, RepositoryDigest: primary.RepositoryDigest, CreatedAt: primary.CreatedAt}
	if err := verify(ctx, target); err != nil {
		return err
	}
	primary.Status, ownership.Status = statusCleanupPending, statusCleanupPending
	primary.CleanupDisposition, ownership.CleanupDisposition = "", ""
	return wt.writeCustodyMarkers(primary, ownership)
}

func archivedStageMarkers(primary, ownership marker, custody StageCustody, directory string) error {
	actual, err := primary.directoryName()
	if err != nil || actual != directory || !sameWorkspaceIdentity(primary, ownership) || primary.StartRef != ownership.StartRef {
		return errors.New("worktree: archived stage markers disagree")
	}
	if primary.RunID != custody.WorkspaceID || primary.OwnerRunID != custody.OwnerRunID || primary.RepositoryDigest != custody.RepositoryDigest || primary.StartRef != custody.StartRef || primary.Branch != custody.Branch {
		return errors.New("worktree: archived stage custody changed")
	}
	for _, mk := range []marker{primary, ownership} {
		switch mk.Status {
		case statusActive, statusCleanupPending:
			if mk.CleanupDisposition != "" {
				return errors.New("worktree: archived stage has unrelated cleanup disposition")
			}
		case statusCleanupRetained:
			if mk.CleanupDisposition != childWaitDisposition {
				return errors.New("worktree: archived stage retained for another reason")
			}
		default:
			return fmt.Errorf("worktree: archived stage cannot release status %q", mk.Status)
		}
	}
	return nil
}
