package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RetireHeldStage removes an exact parent checkout after the caller has durably
// retained and verified its final contribution. Released or absent custody is
// replayable; a changed owner, branch, registration or surviving partial state
// refuses. This never creates a replacement checkout.
func (m *Manager) RetireHeldStage(ctx context.Context, repoURL string, custody StageCustody) error {
	if !validRunID(custody.WorkspaceID) || !validRunID(custody.OwnerRunID) || custody.RepositoryDigest != RepositoryDigest(repoURL) {
		return fmt.Errorf("worktree: invalid parent retirement custody")
	}
	key := repoKey(repoURL)
	path := filepath.Join(m.runsDirForKey(key), worktreeDirectoryName(custody.WorkspaceID))
	absent := false
	found, err := m.WithExistingMirror(ctx, repoURL, func(repository string) error {
		var checkErr error
		absent, checkErr = m.retiredStageAbsent(ctx, repository, key, path, custody)
		return checkErr
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("worktree: parent retirement mirror is unavailable")
	}
	if absent {
		return nil
	}
	wt, err := m.adoptHeldStage(ctx, repoURL, custody, true)
	if err != nil {
		return err
	}
	if err := m.releaseRetiringStage(ctx, wt); err != nil {
		return err
	}
	return wt.Remove(ctx, RemoveOptions{})
}

func (m *Manager) releaseRetiringStage(ctx context.Context, wt *Worktree) error {
	found, err := m.WithExistingMirror(ctx, wt.repoURL, func(string) error {
		primary, ownership, err := wt.custodyMarkers()
		if err != nil {
			return err
		}
		if (primary.Status == statusActive || primary.Status == statusCleanupPending) && primary.CleanupDisposition == "" {
			return nil
		}
		if primary.Status != statusCleanupRetained || primary.CleanupDisposition != childWaitDisposition {
			return fmt.Errorf("worktree: parent retirement hold changed")
		}
		primary.Status, ownership.Status = statusActive, statusActive
		primary.CleanupDisposition, ownership.CleanupDisposition = "", ""
		return wt.writeCustodyMarkers(primary, ownership)
	})
	if err == nil && !found {
		return fmt.Errorf("worktree: parent retirement mirror disappeared")
	}
	return err
}

// Finish only exact leftover records after Git has removed the checkout. A
// missing primary record alone cannot hide a changed ownership record.
func (m *Manager) retiredStageAbsent(ctx context.Context, repository, key, path string, custody StageCustody) (bool, error) {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	registered, err := worktreeRegistered(ctx, repository, path)
	if err != nil || registered {
		return false, fmt.Errorf("worktree: parent retirement has surviving registration")
	}
	paths := []string{m.markerPath(key, custody.WorkspaceID), m.ownershipPath(key, filepath.Base(path))}
	for _, markerPath := range paths {
		value, err := readMarker(markerPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !stageCustodyMatches(value, custody) || !stageRetirementStatus(value, true) {
			return false, fmt.Errorf("worktree: parent retirement records changed")
		}
	}
	for _, markerPath := range paths {
		if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return true, nil
}
