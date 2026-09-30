package telemetry

// The bounded bootstrap area is independent of the rebuildable replay index.
// It retains startup records while an existing backlog is being reconciled.
// Files have the ordinary replay format and are migrated by atomic rename;
// neither admission nor migration uploads them directly.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

var errAzureBootstrapReady = errors.New("azure replay index ready during bootstrap admission")

const azureBootstrapFileLimit = 2048
const azureBootstrapByteLimit = 8 << 20
const azureBootstrapSweepInterval = 30 * time.Second

func bootstrapDir(root, stream string) string {
	if stream == "" {
		stream = "export"
	}
	return filepath.Join(root, ".replay-bootstrap", stream)
}

// The active daemon must find bootstrap files left by another short-lived
// process. Only directory names are inspected on this idle cadence; a full
// manifest audit is not started when the bootstrap area is empty.
func (x *azureReplayIndex) hasBootstrapFiles() bool {
	for _, stream := range x.streams {
		entries, err := os.ReadDir(bootstrapDir(x.root, stream))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return true
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
				return true
			}
		}
	}
	return false
}

func lockBootstrap(ctx context.Context, root string) (func(), error) {
	base := filepath.Join(root, ".replay-bootstrap")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(base)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid Azure bootstrap directory: %s", base)
	}
	path := filepath.Join(base, ".lock")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := platformlock.TryAcquire(path)
		if err == nil {
			if err = lock.File().Chmod(0o600); err != nil {
				_ = lock.Release()
				return nil, err
			}
			return func() { _ = lock.Release() }, nil
		}
		if !errors.Is(err, platformlock.ErrHeld) {
			return nil, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

func ensureBootstrapStream(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid Azure bootstrap directory: %s", dir)
	}
	return nil
}

func (s *azureReplaySpool) submitBootstrap(ctx context.Context, name string, createdAt time.Time, payload []byte) error {
	dir := bootstrapDir(s.index.root, s.stream)
	unlock, err := lockBootstrap(ctx, s.index.root)
	if err != nil {
		return err
	}
	defer unlock()
	if !s.index.bootstrapOpen.Load() {
		return errAzureBootstrapReady
	}
	if s.closed.Load() {
		return errors.New("azure monitor replay spool is closed")
	}
	if err := ensureBootstrapStream(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var count int
	var bytes int64
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return fmt.Errorf("invalid Azure bootstrap entry %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("invalid Azure bootstrap file %q", entry.Name())
		}
		count++
		bytes += info.Size()
	}
	limit := min(s.cfg.maxBytes, int64(azureBootstrapByteLimit))
	// A conservative bound avoids consuming the whole ordinary stream budget
	// while the old backlog is unknown. Exact quota/pruning resumes on migration.
	limit = min(limit, max(int64(1<<20), s.cfg.maxBytes/16))
	if count >= azureBootstrapFileLimit || bytes+int64(len(payload))+azureReplayHeaderLimit > limit {
		return errors.New("azure replay bootstrap bound reached")
	}
	if _, err = writeAzureReplayBatch(dir, name, createdAt, payload); err != nil {
		return err
	}
	s.accepted.Add(uint64(azureReplayRecordCount(payload)))
	s.signal()
	return nil
}

func (x *azureReplayIndex) migrateBootstrap(ctx context.Context) error {
	if !x.bootstrapOpen.Load() && !x.hasBootstrapFiles() {
		return nil
	}
	unlock, err := lockBootstrap(ctx, x.root)
	if err != nil {
		return err
	}
	defer unlock()
	for _, stream := range x.streams {
		if err := x.migrateBootstrapStreamLocked(ctx, stream); err != nil {
			return err
		}
	}
	// Closing admission under the same root bootstrap lock prevents a producer
	// from publishing after its stream was scanned but before index readiness.
	x.bootstrapOpen.Store(false)
	return nil
}

func (x *azureReplayIndex) migrateBootstrapStreamLocked(ctx context.Context, stream string) error {
	dir := bootstrapDir(x.root, stream)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := ensureBootstrapStream(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var files []os.DirEntry
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("invalid Azure bootstrap entry %q", entry.Name())
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("invalid Azure bootstrap file %q", entry.Name())
			}
			files = append(files, entry)
		}
	}
	if len(files) == 0 {
		return nil
	}
	unlockIndex, err := x.lock(ctx)
	if err != nil {
		return err
	}
	defer unlockIndex()
	// Persist a required full audit before any move. A crash after a rename but
	// before its manifest transaction leaves the authoritative file discoverable.
	if _, err = x.db.ExecContext(ctx, `UPDATE reconciliation SET audited=0 WHERE id=1`); err != nil {
		return err
	}
	err = x.transaction(ctx, func(tx *sql.Tx) error {
		for _, entry := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			from := filepath.Join(dir, entry.Name())
			to := filepath.Join(x.root, stream, entry.Name())
			if _, err := os.Lstat(to); err == nil {
				return fmt.Errorf("azure bootstrap destination already exists: %s", entry.Name())
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			created, records, readErr := readAzureReplayMetadata(from)
			if readErr != nil {
				created, records = time.Time{}, 0
			}
			if err := os.Rename(from, to); err != nil {
				return err
			}
			x.dirty = true
			info, err := os.Stat(to)
			if err != nil {
				return err
			}
			f := indexedReplayFile{stream: stream, name: entry.Name(), bytes: info.Size(),
				modified: info.ModTime().UnixNano(), created: created.UnixNano(), records: records}
			if readErr != nil {
				f.created = 0
			}
			if err := x.put(ctx, tx, f); err != nil {
				return err
			}
		}
		if err := x.stamp(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE reconciliation SET audited=? WHERE id=1`, time.Now().UnixNano())
		return err
	})
	if err == nil {
		x.dirty = false
	}
	return err
}
