package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// ReapResult reports a committed retirement, not a new eligibility decision.
type ReapResult struct {
	Path    string
	Deleted bool
	DryRun  bool
	Err     error
}

// ReapRetired finishes file removal for durably retired snapshots. It never
// removes active snapshots or Git refs. deleteFiles=false only reports candidates.
// Failed deletions remain bounded inventory entries and are returned as errors
// as well as per-candidate results, so unattended callers cannot lose failures.
func ReapRetired(ctx context.Context, root string, maxEntries int, deleteFiles bool) ([]ReapResult, error) {
	if maxEntries <= 0 || maxEntries > 10000 {
		return nil, fmt.Errorf("invalid recovery inventory reap limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !before.IsDir() {
		return nil, fmt.Errorf("recovery inventory must be a real directory")
	}
	lock, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()
	names, err := readInventoryNames(root, before, maxEntries)
	if err != nil {
		return nil, err
	}
	var results []ReapResult
	var failures error
	for _, name := range names {
		if !isRetiredName(name) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return results, errors.Join(failures, err)
		}
		result := ReapResult{Path: filepath.Join(root, name), DryRun: !deleteFiles}
		result.Err = validateRetiredDirectory(root, name)
		if result.Err == nil {
			result.Err = validateRetiredFiles(result.Path)
		}
		if result.Err == nil && deleteFiles {
			result.Err = removeRetiredFiles(ctx, root, name)
			result.Deleted = result.Err == nil
		}
		if result.Err != nil {
			failures = errors.Join(failures, fmt.Errorf("reap retired recovery %s: %w", name, result.Err))
		}
		results = append(results, result)
	}
	return results, failures
}

// Caller holds the inventory lock. Remove only known regular files, never
// recurse or follow links. Preflight every file before removing any of them.
func removeRetiredFiles(ctx context.Context, root, name string) error {
	directory := filepath.Join(root, name)
	// Retirement's rename may have succeeded while its directory flush failed.
	// Make that transition durable before removing any archive bytes; otherwise
	// a crash could resurrect an active reservation with its archive deleted.
	if err := durability.SyncDir(root); err != nil {
		return err
	}
	for _, file := range retiredFileNames() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := durability.RemoveFile(filepath.Join(directory, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(directory); err != nil {
		return err
	}
	return durability.SyncDir(root)
}

func retiredFileNames() []string {
	return []string{BundleFileName, retentionFileName, RecordFileName, BundleFileName + ".lock", RecordFileName + ".lock", ".publish.lock"}
}

func validateRetiredFiles(directory string) error {
	for _, file := range retiredFileNames() {
		info, err := os.Lstat(filepath.Join(directory, file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("retired recovery contains non-regular file %s", file)
		}
	}
	return nil
}
