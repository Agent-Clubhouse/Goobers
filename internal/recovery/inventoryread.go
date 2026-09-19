package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// MaxInventoryEntries is the largest inventory that recovery publication and
// readers accept. It is also the independent structural ceiling for recovery
// observations reconstructed from journals. Operator policy may choose any
// smaller bound through retention.recovery.maxSnapshots.
const MaxInventoryEntries = 10000

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
	entries, _, err := readInventory(ctx, root, maxEntries, false)
	return entries, err
}

// UnreadableEntry names a reservation a tolerant scan could not interpret.
type UnreadableEntry struct {
	Name string
	Err  error
}

// ReadInventoryTolerant returns every reservation it CAN read plus the ones it
// could not, instead of failing the whole scan for one broken entry.
//
// ReadInventory is deliberately all-or-nothing, so no caller mistakes an
// incomplete scan for absence of recovery state. That is right for anything
// deciding whether work is safe to discard, and wrong for the one caller that
// is looking for something to evict: a single unreadable reservation — a
// crashed publish leaves a directory holding only lock files — otherwise makes
// eviction return an error forever, so capacity is never reclaimed and the
// inventory grows without bound (#5092, MEASURED on the goobernetes cluster:
// 7 such reservations, inventory climbing ~120/h with nothing ever evicted).
//
// A broken reservation is never itself an eviction candidate, so skipping it
// costs the caller nothing; it is returned rather than discarded so callers
// can still report or reconcile it.
func ReadInventoryTolerant(ctx context.Context, root string, maxEntries int) ([]InventoryEntry, []UnreadableEntry, error) {
	return readInventory(ctx, root, maxEntries, true)
}

// inventoryOccupancy counts the reservations holding a slot, including retired
// and unreadable ones, without the cap refusal readInventoryNames applies.
//
// It is what capacity reclamation asks between retirements: the question "is
// there room yet" cannot be answered by a read that refuses whenever the
// answer is no (#5354).
func inventoryOccupancy(ctx context.Context, root string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	before, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil || !before.IsDir() {
		return 0, fmt.Errorf("recovery inventory must be a real directory")
	}
	handle, err := acquireInventoryLock(ctx, root)
	if err != nil {
		return 0, err
	}
	defer func() { _ = handle.Release() }()
	names, err := listReservationNames(root, before)
	if err != nil {
		return 0, err
	}
	count := len(names)
	if slices.Contains(names, ".inventory.lock") {
		count--
	}
	return count, nil
}

func readInventory(ctx context.Context, root string, maxEntries int, tolerant bool) ([]InventoryEntry, []UnreadableEntry, error) {
	if maxEntries <= 0 || maxEntries > MaxInventoryEntries {
		return nil, nil, fmt.Errorf("invalid recovery inventory read limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	before, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil || !before.IsDir() {
		return nil, nil, fmt.Errorf("recovery inventory must be a real directory")
	}
	handle, err := acquireInventoryLock(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = handle.Release() }()
	names, err := readInventoryNames(root, before, maxEntries)
	if err != nil {
		return nil, nil, err
	}
	var entries []InventoryEntry
	var unreadable []UnreadableEntry
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if name == ".inventory.lock" {
			continue
		}
		if isRetiredName(name) {
			if err := validateRetiredDirectory(root, name); err != nil {
				if !tolerant {
					return nil, nil, err
				}
				unreadable = append(unreadable, UnreadableEntry{Name: name, Err: err})
			}
			continue
		}
		entry, err := readInventoryEntry(root, name)
		if err != nil {
			if !tolerant {
				return nil, nil, fmt.Errorf("inspect recovery reservation %s: %w", name, err)
			}
			unreadable = append(unreadable, UnreadableEntry{Name: name, Err: err})
			continue
		}
		entries = append(entries, entry)
	}
	return entries, unreadable, nil
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
		return nil, fmt.Errorf("%w: %d of %d slots used in %s", ErrInventoryFull, count, limit, root)
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
