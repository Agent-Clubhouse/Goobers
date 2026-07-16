package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FinalizeResult reports how terminal cleanup handled one owned worktree.
type FinalizeResult struct {
	WorktreeID string
	Path       string
	Kept       bool
}

// FinalizeRun removes every worktree owned by runID across all managed repos.
// Kept worktrees survive and are returned with Kept set. The operation is
// idempotent so terminal recovery may invoke it more than once.
func (m *Manager) FinalizeRun(ctx context.Context, runID string) ([]FinalizeResult, error) {
	if !validRunID(runID) {
		return nil, fmt.Errorf("worktree: runID %q must be a single path segment (no \"..\", no \"/\")", runID)
	}
	repoDirs, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worktree: list root %s: %w", m.Root, err)
	}

	var results []FinalizeResult
	var finalizeErr error
	for _, repoDir := range repoDirs {
		if !repoDir.IsDir() {
			continue
		}
		found, err := m.finalizeRepoRun(ctx, repoDir.Name(), runID)
		results = append(results, found...)
		finalizeErr = errors.Join(finalizeErr, err)
	}
	return results, finalizeErr
}

func (m *Manager) finalizeRepoRun(ctx context.Context, key, runID string) ([]FinalizeResult, error) {
	candidates := make(map[string]struct{})
	if entries, err := os.ReadDir(m.runsDirForKey(key)); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				candidates[entry.Name()] = struct{}{}
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("worktree: list runs for %s: %w", key, err)
	}
	if entries, err := os.ReadDir(m.markersDirForKey(key)); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				candidates[strings.TrimSuffix(entry.Name(), ".json")] = struct{}{}
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("worktree: list markers for %s: %w", key, err)
	}

	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	var results []FinalizeResult
	var finalizeErr error
	for _, worktreeID := range ids {
		markerPath := m.markerPath(key, worktreeID)
		mk, markerErr := readMarker(markerPath)
		switch {
		case markerErr == nil:
			if !ownedByRun(mk, worktreeID, runID) {
				continue
			}
			if mk.Status == statusKept {
				results = append(results, FinalizeResult{
					WorktreeID: worktreeID,
					Path:       filepath.Join(m.runsDirForKey(key), worktreeID),
					Kept:       true,
				})
				continue
			}
			if mk.Status != statusActive {
				finalizeErr = errors.Join(finalizeErr,
					fmt.Errorf("worktree: finalize run %s: marker %s has unknown status %q", runID, markerPath, mk.Status))
				continue
			}
		case os.IsNotExist(markerErr):
			if !legacyWorktreeOwnedByRun(worktreeID, runID) {
				continue
			}
		default:
			if legacyWorktreeOwnedByRun(worktreeID, runID) {
				finalizeErr = errors.Join(finalizeErr,
					fmt.Errorf("worktree: finalize run %s: read marker %s: %w", runID, markerPath, markerErr))
			}
			continue
		}

		path := filepath.Join(m.runsDirForKey(key), worktreeID)
		if err := m.forceClear(ctx, key, path); err != nil {
			finalizeErr = errors.Join(finalizeErr,
				fmt.Errorf("worktree: finalize run %s worktree %s: %w", runID, worktreeID, err))
			continue
		}
		results = append(results, FinalizeResult{WorktreeID: worktreeID, Path: path})
	}
	return results, finalizeErr
}

func ownedByRun(mk marker, worktreeID, runID string) bool {
	if mk.OwnerRunID != "" {
		return mk.OwnerRunID == runID
	}
	return legacyWorktreeOwnedByRun(worktreeID, runID)
}

func legacyWorktreeOwnedByRun(worktreeID, runID string) bool {
	return worktreeID == runID || strings.HasPrefix(worktreeID, runID+"-")
}
