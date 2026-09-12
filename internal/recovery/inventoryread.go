package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// InventoryEntry is validated metadata in its identity-bound reservation.
// It is not evidence of archive integrity; import must still verify the bundle.
type InventoryEntry struct {
	Record     Record
	RecordPath string
}

// ReadInventory reads at most maxEntries reservations under the publication
// lock. Unknown, partial, corrupt or misfiled entries produce no partial result:
// callers must not interpret an incomplete scan as absence of recovery state.
// Missing inventories are empty and are not created by this read.
func ReadInventory(ctx context.Context, root string, maxEntries int) ([]InventoryEntry, error) {
	if maxEntries <= 0 || maxEntries > 10000 {
		return nil, fmt.Errorf("invalid recovery inventory read limit")
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
	handle, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Release() }()
	names, err := readInventoryNames(root, before, maxEntries)
	if err != nil {
		return nil, err
	}
	var entries []InventoryEntry
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name == ".inventory.lock" {
			continue
		}
		if isRetiredName(name) {
			if err := validateRetiredDirectory(root, name); err != nil {
				return nil, err
			}
			continue
		}
		entry, err := readInventoryEntry(root, name)
		if err != nil {
			return nil, fmt.Errorf("inspect recovery reservation %s: %w", name, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func readInventoryNames(root string, before os.FileInfo, limit int) ([]string, error) {
	file, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, fmt.Errorf("recovery inventory changed while opening")
	}
	names, err := file.Readdirnames(limit + 2) // one lock plus one overflow probe
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	count := len(names)
	if slices.Contains(names, ".inventory.lock") {
		count--
	}
	if count > limit {
		return nil, ErrInventoryFull
	}
	slices.Sort(names)
	return names, nil
}

func readInventoryEntry(root, name string) (InventoryEntry, error) {
	directory := filepath.Join(root, name)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return InventoryEntry{}, fmt.Errorf("reservation must be a real directory")
	}
	if err := validateReservationContents(directory); err != nil {
		return InventoryEntry{}, err
	}
	path := filepath.Join(directory, RecordFileName)
	record, err := ReadRecord(path)
	if err != nil {
		return InventoryEntry{}, err
	}
	if name != inventoryDirectoryName(record) {
		return InventoryEntry{}, ErrRecordConflict
	}
	archive, err := os.Lstat(filepath.Join(directory, BundleFileName))
	if err != nil || !archive.Mode().IsRegular() || archive.Size() != record.ArchiveBytes {
		return InventoryEntry{}, fmt.Errorf("record has no size-matching regular archive")
	}
	return InventoryEntry{Record: record, RecordPath: path}, nil
}
