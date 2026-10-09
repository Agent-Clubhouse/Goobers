package worktree

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/workspacedelta"
)

const childWaitDisposition = "child-workflow-wait"

// StageCustody identifies an existing held checkout, without a path or clone
// URL. It is recorded only by the host after its writers have joined.
type StageCustody struct {
	WorkspaceID      string `json:"workspaceId"`
	OwnerRunID       string `json:"ownerRunId"`
	RepositoryDigest string `json:"repositoryDigest"`
	Branch           string `json:"branch"`
	StartRef         string `json:"startRef"`
}

// HoldForChild protects an exact checkout from crash reaping, terminal cleanup,
// and age pruning while a child holds custody. The active family owns this
// retention; ReleaseChildHold explicitly hands it back to ordinary cleanup.
func (wt *Worktree) HoldForChild(ctx context.Context) (StageCustody, error) {
	var custody StageCustody
	found, err := wt.manager.WithExistingMirror(ctx, wt.repoURL, func(repository string) error {
		primary, ownership, err := wt.custodyMarkers()
		if err != nil {
			return err
		}
		if primary.Status != statusActive && (primary.Status != statusCleanupRetained || primary.CleanupDisposition != childWaitDisposition) {
			return fmt.Errorf("worktree: stage custody is unavailable")
		}
		custody = StageCustody{WorkspaceID: wt.RunID, OwnerRunID: primary.OwnerRunID, RepositoryDigest: primary.RepositoryDigest, Branch: primary.Branch, StartRef: primary.StartRef}
		ownership.StartRef = primary.StartRef
		primary.Status, ownership.Status = statusCleanupRetained, statusCleanupRetained
		primary.CleanupDisposition, ownership.CleanupDisposition = childWaitDisposition, childWaitDisposition
		return wt.writeCustodyMarkers(primary, ownership)
	})
	if err == nil && !found {
		err = fmt.Errorf("worktree: held stage mirror is unavailable")
	}
	return custody, err
}

// AdoptHeldStage verifies custody without creating, fetching, or resetting a
// checkout. The caller already owns the run execution lease.
func (m *Manager) AdoptHeldStage(ctx context.Context, repoURL string, custody StageCustody) (*Worktree, error) {
	if !validRunID(custody.WorkspaceID) || !validRunID(custody.OwnerRunID) || custody.RepositoryDigest != RepositoryDigest(repoURL) || custody.Branch == "" || custody.StartRef == "" {
		return nil, fmt.Errorf("worktree: invalid held stage custody")
	}
	key := repoKey(repoURL)
	wt := &Worktree{RunID: custody.WorkspaceID, Path: filepath.Join(m.runsDirForKey(key), worktreeDirectoryName(custody.WorkspaceID)), Branch: custody.Branch, manager: m, key: key, startRef: custody.StartRef, repoURL: repoURL}
	found, err := m.WithExistingMirror(ctx, repoURL, func(repository string) error {
		primary, _, err := wt.custodyMarkers()
		if err != nil {
			return err
		}
		if primary.Status != statusCleanupRetained || primary.CleanupDisposition != childWaitDisposition || primary.OwnerRunID != custody.OwnerRunID || primary.RepositoryDigest != custody.RepositoryDigest || primary.StartRef != custody.StartRef || primary.Branch != custody.Branch {
			return fmt.Errorf("worktree: held stage identity has changed")
		}
		registered, err := worktreeRegistered(ctx, repository, wt.Path)
		if err != nil || !registered {
			return fmt.Errorf("worktree: held stage registration is unavailable")
		}
		branch, err := gitOutput(ctx, wt.Path, "symbolic-ref", "--quiet", "HEAD")
		if err != nil || branch != "refs/heads/"+custody.Branch {
			return fmt.Errorf("worktree: held stage branch has changed")
		}
		ancestor, err := workspacedelta.IsAncestor(ctx, mirrorGit{}, wt.Path, custody.StartRef, "HEAD")
		if err != nil || !ancestor {
			return fmt.Errorf("worktree: held stage no longer descends from its original revision")
		}
		wt.assetGuard = primary.AssetPathGuard
		wt.partialMirror = m.partialClone && mirrorIsPartial(ctx, repository)
		return nil
	})
	if err == nil && !found {
		err = fmt.Errorf("worktree: held stage mirror is unavailable")
	}
	return wt, err
}

// ReleaseChildHold re-enables ordinary cleanup after the host has durably
// settled its child custody. It does not reset or remove repository files.
func (wt *Worktree) ReleaseChildHold(ctx context.Context) error {
	found, err := wt.manager.WithExistingMirror(ctx, wt.repoURL, func(string) error {
		primary, ownership, err := wt.custodyMarkers()
		if err != nil {
			return err
		}
		if primary.Status != statusCleanupRetained || primary.CleanupDisposition != childWaitDisposition {
			return fmt.Errorf("worktree: checkout is not held for a child")
		}
		primary.Status, ownership.Status = statusActive, statusActive
		primary.CleanupDisposition, ownership.CleanupDisposition = "", ""
		return wt.writeCustodyMarkers(primary, ownership)
	})
	if err == nil && !found {
		err = fmt.Errorf("worktree: held stage mirror is unavailable")
	}
	return err
}

func (wt *Worktree) custodyMarkers() (marker, marker, error) {
	primary, err := readMarker(wt.manager.markerPath(wt.key, wt.RunID))
	if err != nil {
		return marker{}, marker{}, err
	}
	ownership, err := readMarker(wt.manager.ownershipPath(wt.key, filepath.Base(wt.Path)))
	if err != nil {
		return marker{}, marker{}, err
	}
	directory, directoryErr := primary.directoryName()
	if directoryErr != nil || directory != filepath.Base(wt.Path) || primary.RepositoryDigest != RepositoryDigest(wt.repoURL) {
		return marker{}, marker{}, fmt.Errorf("worktree: stage custody repository or directory changed")
	}
	if !sameWorkspaceIdentity(primary, ownership) || primary.Status != ownership.Status || (ownership.StartRef != "" && primary.StartRef != ownership.StartRef) || primary.CleanupDisposition != ownership.CleanupDisposition || primary.RunID != wt.RunID || primary.Branch != wt.Branch {
		return marker{}, marker{}, fmt.Errorf("worktree: stage custody markers disagree")
	}
	return primary, ownership, nil
}

func (wt *Worktree) writeCustodyMarkers(primary, ownership marker) error {
	if err := writeMarker(wt.manager.ownershipPath(wt.key, filepath.Base(wt.Path)), ownership); err != nil {
		return err
	}
	return writeMarker(wt.manager.markerPath(wt.key, wt.RunID), primary)
}
