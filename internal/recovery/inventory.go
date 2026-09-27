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
	if maxSnapshots <= 0 || maxSnapshots > MaxInventoryEntries || maxArchiveBytes <= 0 {
		return Record{}, "", fmt.Errorf("invalid recovery inventory limits")
	}
	if err := requireIndependentArchive(root, append([]string{repository}, cleanupRoots...), len(cleanupRoots) > 0); err != nil {
		return Record{}, "", err
	}
	name := inventoryDirectoryName(prepared)
	directory, err := lockedReserveSnapshotDirectory(ctx, root, name, maxSnapshots)
	if errors.Is(err, ErrInventoryFull) {
		if freed, evictErr := reclaimInventoryCapacity(ctx, root, maxSnapshots, evict); evictErr == nil && freed {
			if err := ctx.Err(); err != nil {
				return Record{}, "", err
			}
			directory, err = lockedReserveSnapshotDirectory(ctx, root, name, maxSnapshots)
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
func lockedReserveSnapshotDirectory(ctx context.Context, root, name string, limit int) (string, error) {
	handle, err := acquireInventoryLock(ctx, root)
	if err != nil {
		return "", err
	}
	defer func() { _ = handle.Release() }()
	return reserveSnapshotDirectory(root, name, limit)
}

// reclaimInventoryCapacity reclaims in cheapest-and-safest-first order:
// stale incomplete reservations, then the in-package reap of already
// retired-but-unremoved entries, then the caller-supplied evict hook, which
// knows how to retire a terminal-and-landed entry on the spot (#4823).
//
// Incomplete reservations go first because they are the only class whose
// reclamation cannot lose anything: they hold no record and therefore no
// recoverable identity, so unlike eviction there is no landing to prove and
// no policy to consult. Running them here, under actual capacity pressure and
// ahead of the evict hook, is what heals an instance whose inventory has
// already filled with crash debris: its first refused cleanup after upgrade
// clears the debris and succeeds, with no operator action (#5354). Like
// eviction, this deliberately bypasses the periodic sweep's dry-run and
// first-enable gating — those govern a background policy decision, while this
// is the one thing standing between the caller and ErrInventoryFull.
func reclaimInventoryCapacity(ctx context.Context, root string, limit int, evict EvictFunc) (bool, error) {
	freed, failures := reconcileForCapacity(ctx, root, limit)
	reaped, reapErr := reapForCapacity(ctx, root, limit)
	freed, failures = freed || reaped, errors.Join(failures, reapErr)
	if evict == nil {
		return freed, failures
	}
	evictedMore, err := evictForCapacity(ctx, root, limit, evict)
	return freed || evictedMore, errors.Join(failures, err)
}

// evictForCapacity retires reclaimable entries until the inventory has room,
// rather than exactly once.
//
// One retirement is enough only when the inventory sits AT its cap. An
// inventory already holding MORE entries than the cap — what an operator gets
// by lowering maxSnapshots on a full inventory, and the shape of the "130 of
// 128" wedge — is still full after a single retirement, so the publish that
// paid for it was refused anyway and fell through to the overflow tier. The
// surplus then drained at one entry per refused cleanup, if at all (#5354).
//
// The loop is bounded twice over: by the structural ceiling, and by the hook
// itself, which stops the moment it declines to retire anything. It never
// retires past the point where the inventory has room, so an entry is never
// reclaimed for capacity that is already free.
func evictForCapacity(ctx context.Context, root string, limit int, evict EvictFunc) (bool, error) {
	freed := false
	var failures error
	for range MaxInventoryEntries {
		if err := ctx.Err(); err != nil {
			return freed, errors.Join(failures, err)
		}
		occupied, err := inventoryOccupancy(ctx, root)
		if err != nil {
			return freed, errors.Join(failures, err)
		}
		if occupied < limit {
			return freed, failures
		}
		evicted, err := evict(ctx, root, limit)
		if err != nil || !evicted {
			return freed, errors.Join(failures, err)
		}
		freed = true
		// evict() only retires (renames to the .retired- prefix); it does not
		// delete files. A retired entry still counts toward capacity until
		// reaped, so the slot it just freed is not real until this runs.
		if _, reapErr := reapForCapacity(ctx, root, limit); reapErr != nil {
			failures = errors.Join(failures, reapErr)
		}
	}
	return freed, failures
}

func reconcileForCapacity(ctx context.Context, root string, limit int) (bool, error) {
	results, err := ReconcileIncompleteReservations(ctx, root, limit, IncompleteReservationGrace, true)
	freed := false
	for _, result := range results {
		freed = freed || result.Deleted
	}
	return freed, err
}

func reapForCapacity(ctx context.Context, root string, limit int) (bool, error) {
	results, err := ReapRetired(ctx, root, limit, true)
	freed := false
	for _, result := range results {
		freed = freed || result.Deleted
	}
	return freed, err
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
		return "", fmt.Errorf("%w: %d of %d slots used in %s", ErrInventoryFull, count, limit, root)
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
