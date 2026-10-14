package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LoadRetainedParentState resolves the exact host-retained snapshot in either
// durability tier, including promotion from a mirror pin to a bundle. The caller
// owns the managed repository lock. A ref-only record requires its exact live
// pin; a bundled record can never silently downgrade to mirror-only durability.
func LoadRetainedParentState(ctx context.Context, repository, inventoryRoot, overflowRoot string, expected Record, maxBytes int64) (RetainedParentState, error) {
	if err := expected.ValidateRestorable(); err != nil {
		return RetainedParentState{}, err
	}
	source, bundle, err := parentRetentionSource(ctx, inventoryRoot, overflowRoot, expected)
	if err != nil {
		return RetainedParentState{}, err
	}
	if err := matchParentRetention(source, expected, time.Now()); err != nil {
		return RetainedParentState{}, err
	}
	if bundle != "" {
		if err := ImportSnapshotBundle(ctx, repository, bundle, source, maxBytes); err != nil {
			return RetainedParentState{}, err
		}
	} else if !HasSnapshotRef(ctx, repository, source) {
		return RetainedParentState{}, ErrOverflowRefUnresolved
	}
	return ReadRetainedParentState(ctx, repository, expected)
}

func parentRetentionSource(ctx context.Context, inventoryRoot, overflowRoot string, expected Record) (Record, string, error) {
	source, bundle, err := readParentBundleSource(ctx, inventoryRoot, expected)
	if err == nil || !errors.Is(err, os.ErrNotExist) || expected.ArchiveDigest != "" {
		return source, bundle, err
	}
	if err := ctx.Err(); err != nil {
		return Record{}, "", err
	}
	if _, err := parentRetentionDirectory(overflowRoot, expected); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Promotion may have finished between the first bundle lookup and
			// the overflow lookup. One bounded reread recognizes that handoff.
			return readParentBundleSource(ctx, inventoryRoot, expected)
		}
		return Record{}, "", err
	}
	entry, err := readOverflowEntry(overflowRoot, inventoryDirectoryName(expected))
	if errors.Is(err, os.ErrNotExist) {
		return readParentBundleSource(ctx, inventoryRoot, expected)
	}
	return entry.Record, "", err
}

func readParentBundleSource(ctx context.Context, root string, expected Record) (Record, string, error) {
	directory, err := parentRetentionDirectory(root, expected)
	if err != nil {
		return Record{}, "", err
	}
	lease, err := acquireInventoryLock(ctx, root)
	if err != nil {
		return Record{}, "", err
	}
	defer func() { _ = lease.Release() }()
	entry, err := readInventoryEntry(root, inventoryDirectoryName(expected))
	if err != nil {
		return Record{}, "", err
	}
	current, err := ReadRetainedRecord(entry.RecordPath)
	if err != nil {
		return Record{}, "", err
	}
	return current, filepath.Join(directory, BundleFileName), nil
}

func parentRetentionDirectory(root string, record Record) (string, error) {
	if root == "" {
		return "", os.ErrNotExist
	}
	directory := filepath.Join(root, inventoryDirectoryName(record))
	for _, path := range []string{root, directory} {
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("parent retention requires real directories")
		}
	}
	return directory, nil
}

func matchParentRetention(current, expected Record, now time.Time) error {
	if err := current.ValidateRestorable(); err != nil {
		return err
	}
	if current.RetainUntil.Before(expected.RetainUntil) || !now.Before(current.RetainUntil) {
		return errors.New("parent retention expired or moved backwards")
	}
	current.RetainUntil = expected.RetainUntil
	if expected.ArchiveDigest == "" {
		// Promotion adds only the verified bundle binding; the captured work
		// and its source identity must remain exactly the same.
		current.ArchiveDigest, current.ArchiveBytes, current.ArchiveFormat = "", 0, ""
	}
	if current != expected {
		return ErrRecordConflict
	}
	return nil
}
