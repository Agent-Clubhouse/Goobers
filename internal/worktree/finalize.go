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
	WorktreeID         string
	Path               string
	Kept               bool
	CleanupDisposition string
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
			preserved, result, err := m.finalizePreservedMarker(key, worktreeID, mk)
			if err != nil {
				finalizeErr = errors.Join(finalizeErr,
					fmt.Errorf("worktree: finalize run %s: resolve marker %s: %w", runID, markerPath, err))
				continue
			}
			if preserved {
				results = append(results, result)
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

		directory := worktreeID
		if markerErr == nil {
			directory, _ = mk.directoryName()
		}
		path := filepath.Join(m.runsDirForKey(key), directory)
		worktreeBytes, worktreeMeasured, measurementErr := m.measureWorktree(path)
		if err := m.forceClear(ctx, key, path, worktreeID); err != nil {
			m.observeUsage(ctx, UsageOperationTeardown, runID, worktreeID, worktreeBytes, worktreeMeasured, measurementErr)
			if result, retained := finalizeRetainedResult(worktreeID, path, err); retained {
				results = append(results, result)
				continue
			}
			finalizeErr = errors.Join(finalizeErr,
				fmt.Errorf("worktree: finalize run %s worktree %s: %w", runID, worktreeID, err))
			continue
		}
		m.observeUsage(ctx, UsageOperationTeardown, runID, worktreeID, worktreeBytes, worktreeMeasured, measurementErr)
		results = append(results, FinalizeResult{WorktreeID: worktreeID, Path: path})
	}
	// Failed handoffs leave worktrees alive. Keep their branch-acquisition
	// evidence too, so retries cannot treat retained work as a fresh branch.
	if finalizeErr != nil {
		return results, finalizeErr
	}

	acquisitionDir := m.branchAcquisitionRunDir(key, runID)
	if err := os.RemoveAll(acquisitionDir); err != nil {
		finalizeErr = errors.Join(finalizeErr,
			fmt.Errorf("worktree: finalize run %s: remove branch acquisitions: %w", runID, err))
	} else if err := fsyncDir(filepath.Dir(acquisitionDir)); err != nil && !os.IsNotExist(err) {
		finalizeErr = errors.Join(finalizeErr,
			fmt.Errorf("worktree: finalize run %s: sync branch acquisitions: %w", runID, err))
	}
	return results, finalizeErr
}

func (m *Manager) finalizePreservedMarker(key, worktreeID string, mk marker) (bool, FinalizeResult, error) {
	directory, err := mk.directoryName()
	if err != nil {
		return false, FinalizeResult{}, err
	}
	result := FinalizeResult{WorktreeID: worktreeID, Path: filepath.Join(m.runsDirForKey(key), directory)}
	switch mk.Status {
	case statusKept:
		result.Kept = true
		return true, result, nil
	case statusCleanupRetained:
		result.CleanupDisposition = mk.CleanupDisposition
		return true, result, nil
	case statusActive, statusCleanupPending:
		return false, result, nil
	default:
		return false, FinalizeResult{}, fmt.Errorf("marker has unknown status %q", mk.Status)
	}
}

func finalizeRetainedResult(worktreeID, path string, err error) (FinalizeResult, bool) {
	var retained *CleanupRetentionError
	if !errors.Is(err, ErrCleanupRetained) || !errors.As(err, &retained) {
		return FinalizeResult{}, false
	}
	return FinalizeResult{
		WorktreeID:         worktreeID,
		Path:               path,
		CleanupDisposition: retained.Disposition,
	}, true
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
