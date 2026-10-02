package worktree

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// BranchOccupancy describes the durable owner of a branch currently checked
// out in a managed worktree.
type BranchOccupancy struct {
	OwnerRunID string
	Status     BranchOccupancyStatus
}

// BranchOccupancyStatus is the durable lifecycle state recorded for an
// occupied branch.
type BranchOccupancyStatus string

const (
	// BranchOccupancyActive means the owning stage has not surrendered its
	// workspace.
	BranchOccupancyActive BranchOccupancyStatus = "active"
)

// BranchOccupancies returns the branches currently checked out for repoURL.
// It validates both ownership records before exposing their state so callers
// never make scheduling decisions from stale or ambiguous metadata.
func (m *Manager) BranchOccupancies(ctx context.Context, repoURL string) (map[string]BranchOccupancy, error) {
	key := repoKey(repoURL)
	repoDir := m.repoDirForKey(key)
	if _, err := os.Stat(repoDir); err != nil {
		if os.IsNotExist(err) {
			return map[string]BranchOccupancy{}, nil
		}
		return nil, fmt.Errorf("worktree: inspect repository for branch occupancy: %w", err)
	}

	entries, err := registeredWorktrees(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("worktree: inspect branch occupancy: %w", err)
	}
	occupancies := make(map[string]BranchOccupancy)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Branch, "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(entry.Branch, "refs/heads/")
		directory, err := m.containedRunDirectory(key, entry.Path)
		if err != nil {
			return nil, fmt.Errorf("worktree: inspect branch %q occupant %s: %w", branch, entry.Path, err)
		}
		ownership, err := readMarker(m.ownershipPath(key, directory))
		if err != nil {
			return nil, fmt.Errorf("worktree: inspect branch %q occupant %s ownership record: %w", branch, entry.Path, err)
		}
		if ownership.Directory == "" || ownership.Directory != directory || !validRunID(ownership.RunID) ||
			worktreeDirectoryName(ownership.RunID) != directory {
			return nil, fmt.Errorf("worktree: inspect branch %q occupant %s: ownership directory identity is invalid", branch, entry.Path)
		}
		primary, err := readMarker(m.markerPath(key, ownership.RunID))
		if err != nil {
			return nil, fmt.Errorf("worktree: inspect branch %q occupant %s run marker: %w", branch, entry.Path, err)
		}
		if !sameWorkspaceIdentity(primary, ownership) {
			return nil, fmt.Errorf("worktree: inspect branch %q occupant %s: ownership records disagree", branch, entry.Path)
		}
		if primary.RepositoryDigest != RepositoryDigest(repoURL) || primary.Branch != branch {
			return nil, fmt.Errorf("worktree: inspect branch %q occupant %s: repository or branch identity disagrees", branch, entry.Path)
		}
		if primary.OwnerRunID == "" {
			return nil, fmt.Errorf("worktree: inspect branch %q occupant %s: owner run ID is missing", branch, entry.Path)
		}
		if _, exists := occupancies[branch]; exists {
			return nil, fmt.Errorf("worktree: branch %q has multiple registered occupants", branch)
		}
		occupancies[branch] = BranchOccupancy{
			OwnerRunID: primary.OwnerRunID,
			Status:     BranchOccupancyStatus(primary.Status),
		}
	}
	return occupancies, nil
}
