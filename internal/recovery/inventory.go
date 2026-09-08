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

// PublishToInventory reserves a deterministic snapshot directory then publishes
// its archive and bound record. root must already exist privately outside all
// cleanup roots. Failed reservations count toward maxSnapshots until explicitly
// reconciled, bounding crash/retry debris as well as successful records. Each
// archive is bounded by maxArchiveBytes; this helper never evicts old records.
func PublishToInventory(ctx context.Context, repository, root string, cleanupRoots []string, prepared Record, maxSnapshots int, maxArchiveBytes int64) (Record, string, error) {
	if err := prepared.validateSnapshot(); err != nil {
		return Record{}, "", err
	}
	if maxSnapshots <= 0 || maxSnapshots > 10000 || maxArchiveBytes <= 0 {
		return Record{}, "", fmt.Errorf("invalid recovery inventory limits")
	}
	if err := requireIndependentArchive(root, append([]string{repository}, cleanupRoots...), len(cleanupRoots) > 0); err != nil {
		return Record{}, "", err
	}
	handle, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		return Record{}, "", err
	}
	defer func() { _ = handle.Release() }()
	directory, err := reserveSnapshotDirectory(root, inventoryDirectoryName(prepared), maxSnapshots)
	if err != nil {
		return Record{}, "", err
	}
	published, err := PublishRetainedState(ctx, repository, directory, cleanupRoots, prepared, maxArchiveBytes)
	if err != nil {
		return Record{}, "", err
	}
	return published, filepath.Join(directory, RecordFileName), nil
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
