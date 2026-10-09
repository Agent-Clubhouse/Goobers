package worktree

import (
	"context"
	"fmt"
	"path/filepath"
)

// StageIdentity reads the exact active or held checkout identity before its host
// owner journals a reservation. It does not grant custody or change Git state.
func (wt *Worktree) StageIdentity(ctx context.Context) (StageCustody, error) {
	var custody StageCustody
	found, err := wt.manager.WithExistingMirror(ctx, wt.repoURL, func(string) error {
		primary, _, err := wt.reservedStageMarkers(true)
		if err != nil {
			return err
		}
		custody = StageCustody{WorkspaceID: wt.RunID, OwnerRunID: primary.OwnerRunID, RepositoryDigest: primary.RepositoryDigest, Branch: primary.Branch, StartRef: primary.StartRef}
		return nil
	})
	if err == nil && !found {
		err = fmt.Errorf("worktree: reserved stage mirror is unavailable")
	}
	return custody, err
}

// HoldReservedStage acquires only the exact journaled checkout, without reset,
// fetch or creation. Both markers and the live registration/branch/ancestry are
// verified under the repository lock before holding it. An interrupted hold's
// matching active/held marker pair can be completed; unrelated states refuse.
func (m *Manager) HoldReservedStage(ctx context.Context, repoURL string, custody StageCustody) (*Worktree, error) {
	return m.adoptReservedStage(ctx, repoURL, custody, true)
}

func heldStageMarker(value marker) bool {
	return value.Status == statusCleanupRetained && value.CleanupDisposition == childWaitDisposition
}

func reservableStageMarker(value marker) bool {
	return (value.Status == statusActive && value.CleanupDisposition == "") || heldStageMarker(value)
}

func (wt *Worktree) reservedStageMarkers(allowInterrupted bool) (marker, marker, error) {
	if !allowInterrupted {
		return wt.custodyMarkers()
	}
	primary, err := readMarker(wt.manager.markerPath(wt.key, wt.RunID))
	if err != nil {
		return marker{}, marker{}, err
	}
	ownership, err := readMarker(wt.manager.ownershipPath(wt.key, filepath.Base(wt.Path)))
	if err != nil {
		return marker{}, marker{}, err
	}
	directory, directoryErr := primary.directoryName()
	if directoryErr != nil || directory != filepath.Base(wt.Path) || primary.RepositoryDigest != RepositoryDigest(wt.repoURL) || primary.RunID != wt.RunID || primary.Branch != wt.Branch {
		return marker{}, marker{}, fmt.Errorf("worktree: reserved stage repository or directory changed")
	}
	if primary.ParentRestoreHead != "" || ownership.ParentRestoreHead != "" || !sameWorkspaceIdentity(primary, ownership) || (ownership.StartRef != "" && primary.StartRef != ownership.StartRef) || !reservableStageMarker(primary) || !reservableStageMarker(ownership) {
		return marker{}, marker{}, fmt.Errorf("worktree: reserved stage markers disagree")
	}
	return primary, ownership, nil
}
