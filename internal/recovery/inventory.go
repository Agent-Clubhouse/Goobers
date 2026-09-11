package recovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// ErrInventoryFull refuses new recovery state without evicting existing
// evidence. Cleanup must preserve its source when allocation fails.
var ErrInventoryFull = errors.New("recovery inventory is full")

// EvictFunc attempts to free at least one inventory slot when reservation
// finds the inventory full, and reports whether it freed anything. It is
// tried only under actual capacity pressure — never speculatively — so it can
// safely bypass the periodic retention sweep's dry-run/first-enable/retain-
// window gating (#4823 AC4): those gate a background policy decision, while
// this is the one thing standing between the caller and ErrInventoryFull. A
// false/error report only costs the caller the reservation it already faced;
// it never causes data loss, since the source is preserved either way.
type EvictFunc func(ctx context.Context, root string, limit int) (bool, error)

// PublishToInventoryWithEviction reserves a deterministic snapshot directory
// then publishes its archive and bound record. root must already exist
// privately outside all cleanup roots. Failed reservations count toward
// maxSnapshots until explicitly reconciled, bounding crash/retry debris as
// well as successful records. Each archive is bounded by maxArchiveBytes.
//
// When the inventory is full, it first reaps any already-retired-but-unreaped
// entries (cheap, no external context needed) and then, if still full, gives
// evict one chance to retire something before failing (#4823). evict may be
// nil, in which case only the reap step runs and it otherwise never evicts.
func PublishToInventoryWithEviction(ctx context.Context, repository, root string, cleanupRoots []string, prepared Record, maxSnapshots int, maxArchiveBytes int64, evict EvictFunc) (Record, string, error) {
	return publishToInventory(ctx, repository, root, cleanupRoots, prepared, maxSnapshots, maxArchiveBytes, nil, evict)
}

func publishToInventory(ctx context.Context, repository, root string, cleanupRoots []string, prepared Record, maxSnapshots int, maxArchiveBytes int64, beforePublish func() error, evict EvictFunc) (Record, string, error) {
	if err := prepared.validateSnapshot(); err != nil {
		return Record{}, "", err
	}
	if maxSnapshots <= 0 || maxSnapshots > 10000 || maxArchiveBytes <= 0 {
		return Record{}, "", fmt.Errorf("invalid recovery inventory limits")
	}
	if err := requireIndependentArchive(root, append([]string{repository}, cleanupRoots...), len(cleanupRoots) > 0); err != nil {
		return Record{}, "", err
	}
	name := inventoryDirectoryName(prepared)
	directory, err := lockedReserveSnapshotDirectory(root, name, maxSnapshots)
	if errors.Is(err, ErrInventoryFull) {
		if freed, evictErr := reclaimInventoryCapacity(ctx, root, maxSnapshots, evict); evictErr == nil && freed {
			if err := ctx.Err(); err != nil {
				return Record{}, "", err
			}
			directory, err = lockedReserveSnapshotDirectory(root, name, maxSnapshots)
		}
	}
	if err != nil {
		return Record{}, "", err
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return Record{}, "", err
		}
	}
	published, err := PublishRetainedState(ctx, repository, directory, cleanupRoots, prepared, maxArchiveBytes)
	if err != nil {
		return Record{}, "", err
	}
	return published, filepath.Join(directory, RecordFileName), nil
}

// lockedReserveSnapshotDirectory holds the inventory lock only across the
// reservation attempt itself, so a subsequent eviction attempt (which
// acquires the same lock through RetireSnapshot/ReapRetired) never deadlocks
// against it.
func lockedReserveSnapshotDirectory(root, name string, limit int) (string, error) {
	handle, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		return "", err
	}
	defer func() { _ = handle.Release() }()
	return reserveSnapshotDirectory(root, name, limit)
}

// reclaimInventoryCapacity always attempts the in-package reap of already
// retired-but-unremoved entries first, then the caller-supplied evict hook,
// which knows how to retire a terminal-and-landed entry on the spot (#4823).
func reclaimInventoryCapacity(ctx context.Context, root string, limit int, evict EvictFunc) (bool, error) {
	reaped, reapErr := ReapRetired(ctx, root, limit, true)
	freed := false
	for _, result := range reaped {
		if result.Deleted {
			freed = true
		}
	}
	if evict == nil {
		return freed, reapErr
	}
	if err := ctx.Err(); err != nil {
		return freed, errors.Join(reapErr, err)
	}
	evictedMore, err := evict(ctx, root, limit)
	if err != nil {
		return freed, errors.Join(reapErr, err)
	}
	if evictedMore {
		// evict() only retires (renames to the .retired- prefix); it does not
		// delete files. A retired entry still counts toward capacity until
		// reaped, so the slot it just freed is not real until this runs.
		second, secondErr := ReapRetired(ctx, root, limit, true)
		for _, result := range second {
			if result.Deleted {
				freed = true
			}
		}
		reapErr = errors.Join(reapErr, secondErr)
	}
	return freed || evictedMore, reapErr
}

func inventoryDirectoryName(record Record) string {
	identity := sha256.Sum256([]byte(record.RepositoryKey + "\x00" + record.RunID + "\x00" + record.SnapshotSHA))
	return fmt.Sprintf("%x", identity)
}

// Caller holds the inventory lock. Count every entry except that lock: unknown
// or partial entries consume capacity and are never silently discarded.
func reserveSnapshotDirectory(root, name string, limit int) (string, error) {
	directory := filepath.Join(root, name)
	if info, err := os.Lstat(directory); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("recovery reservation is not a directory")
		}
		if err := validateReservationContents(directory); err != nil {
			return "", err
		}
		return directory, durability.SyncDir(root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	file, err := os.Open(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	entries, err := file.Readdirnames(limit + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	count := 0
	for _, entry := range entries {
		if entry != ".inventory.lock" {
			count++
		}
	}
	if count >= limit {
		return "", ErrInventoryFull
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", err
	}
	if err := durability.SyncDir(root); err != nil {
		return "", err
	}
	return directory, nil
}

func validateReservationContents(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	entries, err := file.Readdirnames(7)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, entry := range entries {
		switch entry {
		case BundleFileName, RecordFileName, retentionFileName, ".publish.lock", BundleFileName + ".lock", RecordFileName + ".lock":
		default:
			return fmt.Errorf("recovery reservation requires reconciliation before retry")
		}
	}
	return nil
}
