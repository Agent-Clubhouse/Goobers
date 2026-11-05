package recovery_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/test/testsupport/conformance/recoveryindex"
)

// inventoryIndex adapts the local recovery inventory, the reference backend,
// to recovery.RecordIndex: an identity-named reservation per snapshot holding
// the immutable record, its bundle, and the monotone retention sidecar.
type inventoryIndex struct{ root string }

const maxArchiveBytes = 1 << 20

func (i inventoryIndex) recordPath(key recovery.RecordKey) string {
	name := recovery.InventoryDirectoryName(recovery.Record{RepositoryKey: key.RepositoryKey, RunID: key.RunID, SnapshotSHA: key.SnapshotSHA})
	return filepath.Join(i.root, name, recovery.RecordFileName)
}

func indexed(path string, record recovery.Record) recovery.IndexedRecord {
	return recovery.IndexedRecord{Record: record, Bundle: filepath.Join(filepath.Dir(path), recovery.BundleFileName)}
}

func (i inventoryIndex) Publish(ctx context.Context, record recovery.Record, bundle string, retention time.Duration) (recovery.IndexedRecord, error) {
	if !record.RetainUntil.IsZero() || retention <= 0 {
		return recovery.IndexedRecord{}, errors.New("invalid publication")
	}
	record.CreatedAt = record.CreatedAt.UTC()
	record.RetainUntil = time.Now().Add(retention).UTC()
	if err := record.Validate(); err != nil {
		return recovery.IndexedRecord{}, err
	}
	lock, err := recovery.AcquireInventoryLock(ctx, i.root)
	if err != nil {
		return recovery.IndexedRecord{}, err
	}
	defer func() { _ = lock.Release() }()
	path := i.recordPath(record.Key())
	existing, err := recovery.ReadRetainedRecord(path)
	if err == nil {
		if !recovery.SameIdentity(existing, record) {
			return recovery.IndexedRecord{}, recovery.ErrRecordConflict
		}
		return indexed(path, existing), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return recovery.IndexedRecord{}, err
	}
	// A location another publication already moved into place is unavailable.
	if info, err := os.Lstat(bundle); err != nil || !info.Mode().IsRegular() {
		return recovery.IndexedRecord{}, recovery.ErrBundleUnavailable
	}
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		return recovery.IndexedRecord{}, err
	}
	if err := os.Rename(bundle, filepath.Join(filepath.Dir(path), recovery.BundleFileName)); err != nil {
		return recovery.IndexedRecord{}, err
	}
	if err := recovery.PublishRecord(path, record); err != nil {
		return recovery.IndexedRecord{}, err
	}
	return indexed(path, record), nil
}

func (i inventoryIndex) Renew(ctx context.Context, key recovery.RecordKey, retention time.Duration) (recovery.IndexedRecord, error) {
	path := i.recordPath(key)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return recovery.IndexedRecord{}, recovery.ErrRecordNotFound
	}
	record, err := recovery.RenewRetention(ctx, path, time.Now().Add(retention), maxArchiveBytes)
	if err != nil {
		return recovery.IndexedRecord{}, err
	}
	return indexed(path, record), nil
}

func (i inventoryIndex) Get(_ context.Context, key recovery.RecordKey) (recovery.IndexedRecord, error) {
	path := i.recordPath(key)
	record, err := recovery.ReadRetainedRecord(path)
	if errors.Is(err, os.ErrNotExist) {
		return recovery.IndexedRecord{}, recovery.ErrRecordNotFound
	}
	if err != nil {
		return recovery.IndexedRecord{}, err
	}
	return indexed(path, record), nil
}

func (i inventoryIndex) Reap(ctx context.Context, limit int) ([]recovery.IndexedRecord, error) {
	entries, err := recovery.ReadInventory(ctx, i.root, recovery.MaxInventoryEntries)
	if err != nil {
		return nil, err
	}
	var reaped []recovery.IndexedRecord
	for _, entry := range entries {
		if len(reaped) == limit {
			break
		}
		// The inventory entry holds the published deadline; renewals live in
		// the sidecar, so only the retained read is effective.
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return reaped, err
		}
		if !record.RetainUntil.Before(time.Now()) {
			continue
		}
		if _, err := recovery.RetireSnapshot(ctx, i.root, record, func(recovery.Record) error { return nil }); err != nil {
			return reaped, err
		}
		reaped = append(reaped, indexed(entry.RecordPath, record))
	}
	if _, err := recovery.ReapRetired(ctx, i.root, recovery.MaxInventoryEntries, true); err != nil {
		return reaped, err
	}
	return reaped, nil
}

// RetiredBundles is empty: the inventory deletes bundles during Reap, so none
// is ever left awaiting deletion.
func (inventoryIndex) RetiredBundles(context.Context, int) ([]string, error) { return nil, nil }

// ReleaseBundle is a no-op: the inventory deletes bundles during Reap.
func (inventoryIndex) ReleaseBundle(context.Context, string) error { return nil }

func TestRecordIndexConformance(t *testing.T) {
	recoveryindex.Run(t, func(t *testing.T) recoveryindex.Backend {
		root, staging := filepath.Join(t.TempDir(), "inventory"), t.TempDir()
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		return recoveryindex.Backend{
			Open:              func(*testing.T) recovery.RecordIndex { return inventoryIndex{root: root} },
			DeletesDuringReap: true,
			StageBundle: func(t *testing.T, data []byte) string {
				file, err := os.CreateTemp(staging, "bundle-*")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write(data); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				return file.Name()
			},
		}
	})
}
