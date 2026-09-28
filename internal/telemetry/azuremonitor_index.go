package telemetry

// The replay files remain authoritative. This private, rebuildable SQLite
// manifest caches their metadata and coordinates claims across processes. All
// filesystem mutations and manifest changes run under one per-root OS lock;
// HTTP never holds that lock. Directory stamps detect an interrupted mutation
// (or an older writer) and trigger reconciliation, not a scan on every batch.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/sqliteschema"
	"github.com/goobers/goobers/internal/sqliteuri"
)

const azureReplayIndexName = ".replay-index.db"

var azureReplayIndexes = struct {
	sync.Mutex
	roots map[string]*azureReplayIndex
}{roots: make(map[string]*azureReplayIndex)}

type azureReplayIndex struct {
	root      string
	streams   []string
	db        *sql.DB
	ready     chan struct{}
	cancel    context.CancelFunc
	err       error
	refs      int // guarded by azureReplayIndexes
	lockOnce  sync.Once
	localLock chan struct{}
}

func replayIndexLocation(cfg azureReplayConfig) (string, string, error) {
	root, stream := cfg.root, filepath.Base(cfg.dir)
	if root == "" {
		root, stream = cfg.dir, ""
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	// Storage need not exist or be available at daemon startup. OS file locks
	// still coordinate aliases across index handles; initialization retries all
	// filesystem work on its background goroutine.
	return filepath.Clean(absolute), stream, nil
}

func acquireReplayIndex(cfg azureReplayConfig) (*azureReplayIndex, string, error) {
	root, stream, err := replayIndexLocation(cfg)
	if err != nil {
		return nil, "", err
	}
	azureReplayIndexes.Lock()
	defer azureReplayIndexes.Unlock()
	if index := azureReplayIndexes.roots[root]; index != nil {
		index.refs++
		return index, stream, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	index := &azureReplayIndex{root: root, streams: []string{"traces", "journal", "diagnostics"}, ready: make(chan struct{}), cancel: cancel, refs: 1}
	if stream == "" {
		index.streams = []string{""}
	}
	azureReplayIndexes.roots[root] = index
	go func() {
		defer close(index.ready)
		index.err = index.initialize(ctx)
	}()
	return index, stream, nil
}

func (x *azureReplayIndex) initialize(ctx context.Context) error {
	for {
		if err := x.open(ctx); err == nil {
			return nil
		}
		if x.db != nil {
			_ = x.db.Close()
			x.db = nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func (x *azureReplayIndex) release(ctx context.Context) error {
	azureReplayIndexes.Lock()
	x.refs--
	last := x.refs == 0
	if last {
		delete(azureReplayIndexes.roots, x.root)
		x.cancel()
	}
	azureReplayIndexes.Unlock()
	if !last {
		return nil
	}
	select {
	case <-x.ready:
		if x.db != nil {
			return x.db.Close()
		}
	case <-ctx.Done():
		go func() {
			<-x.ready
			if x.db != nil {
				_ = x.db.Close()
			}
		}()
		return ctx.Err()
	}
	return nil
}

func (x *azureReplayIndex) wait(ctx context.Context) error {
	select {
	case <-x.ready:
		return x.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (x *azureReplayIndex) lock(ctx context.Context) (func(), error) {
	// Queue local contenders before polling the cross-process lock. Otherwise a
	// hot drainer can repeatedly reacquire between producers' retry ticks and
	// starve admission even though individual critical sections are short.
	x.lockOnce.Do(func() { x.localLock = make(chan struct{}, 1) })
	select {
	case x.localLock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	transferred := false
	defer func() {
		if !transferred {
			<-x.localLock
		}
	}()
	path := filepath.Join(x.root, ".replay-index.lock")
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
			transferred = true
			return func() { _ = lock.Release(); <-x.localLock }, nil
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

const replayIndexSchema = `
CREATE TABLE IF NOT EXISTS files (
 stream TEXT NOT NULL, name TEXT NOT NULL,
 bytes INTEGER NOT NULL, modified INTEGER NOT NULL, created INTEGER NOT NULL, records INTEGER NOT NULL,
 lease_until INTEGER NOT NULL DEFAULT 0, lease_owner TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(stream,name)
);
CREATE INDEX IF NOT EXISTS replay_order ON files(stream,created,name);
CREATE INDEX IF NOT EXISTS replay_age ON files(created,name);
CREATE INDEX IF NOT EXISTS replay_lease_owner ON files(lease_owner);
CREATE TABLE IF NOT EXISTS totals(stream TEXT PRIMARY KEY, bytes INTEGER NOT NULL, records INTEGER NOT NULL, files INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS directories(stream TEXT PRIMARY KEY, modified INTEGER NOT NULL);
CREATE TRIGGER IF NOT EXISTS replay_insert AFTER INSERT ON files BEGIN
 INSERT INTO totals VALUES(NEW.stream,NEW.bytes,NEW.records,1)
 ON CONFLICT(stream) DO UPDATE SET bytes=bytes+NEW.bytes,records=records+NEW.records,files=files+1;
END;
CREATE TRIGGER IF NOT EXISTS replay_delete AFTER DELETE ON files BEGIN
 UPDATE totals SET bytes=bytes-OLD.bytes,records=records-OLD.records,files=files-1 WHERE stream=OLD.stream;
END;
CREATE TRIGGER IF NOT EXISTS replay_update AFTER UPDATE OF bytes,records ON files BEGIN
 UPDATE totals SET bytes=bytes+NEW.bytes-OLD.bytes,records=records+NEW.records-OLD.records WHERE stream=OLD.stream;
END;`

func (x *azureReplayIndex) open(ctx context.Context) error {
	for _, stream := range x.streams {
		if err := os.MkdirAll(filepath.Join(x.root, stream), 0o700); err != nil {
			return err
		}
	}
	unlock, err := x.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(x.root, azureReplayIndexName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	err = f.Chmod(0o600)
	_ = f.Close()
	if err != nil {
		return err
	}
	x.db, err = sql.Open("sqlite", sqliteuri.File(path)+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(2000)&_txlock=immediate")
	if err != nil {
		return err
	}
	x.db.SetMaxOpenConns(1)
	if err = sqliteschema.Migrate(ctx, x.db, "azure-replay-index", []string{replayIndexSchema}); err != nil {
		return err
	}
	// A fresh manifest or changed directory is reconciled. Opening every short-
	// lived CLI process must not rescan an already-current daemon manifest.
	return x.transaction(ctx, func(tx *sql.Tx) error { return x.reconcile(ctx, tx, false) })
}

func (x *azureReplayIndex) transaction(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (x *azureReplayIndex) withLock(ctx context.Context, f func(*sql.Tx) error) error {
	if err := x.wait(ctx); err != nil {
		return err
	}
	unlock, err := x.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return x.transaction(ctx, func(tx *sql.Tx) error {
		if err := x.reconcile(ctx, tx, false); err != nil {
			return err
		}
		if err := f(tx); err != nil {
			return err
		}
		return x.stamp(ctx, tx)
	})
}

func directoryStamp(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.ModTime().UnixNano(), nil
}

func (x *azureReplayIndex) stamp(ctx context.Context, tx *sql.Tx) error {
	for _, stream := range x.streams {
		stamp, err := directoryStamp(filepath.Join(x.root, stream))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO directories VALUES(?,?) ON CONFLICT(stream) DO UPDATE SET modified=excluded.modified`, stream, stamp); err != nil {
			return err
		}
	}
	return nil
}

type indexedReplayFile struct {
	stream, name             string
	bytes, modified, created int64
	records                  int
}

func (x *azureReplayIndex) path(f indexedReplayFile) (string, error) {
	allowed := false
	for _, s := range x.streams {
		allowed = allowed || s == f.stream
	}
	if !allowed || filepath.Base(f.name) != f.name || strings.ContainsAny(f.name, "/\\") || !strings.HasSuffix(f.name, azureReplayFileSuffix) {
		return "", errors.New("invalid replay manifest path")
	}
	return filepath.Join(x.root, f.stream, f.name), nil
}

func (x *azureReplayIndex) reconcile(ctx context.Context, tx *sql.Tx, force bool) error {
	for _, stream := range x.streams {
		dir := filepath.Join(x.root, stream)
		stamp, err := directoryStamp(dir)
		if err != nil {
			return err
		}
		var prior int64
		err = tx.QueryRowContext(ctx, `SELECT modified FROM directories WHERE stream=?`, stream).Scan(&prior)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !force && err == nil && prior == stamp {
			continue
		}
		if err = x.reconcileDirectory(ctx, tx, stream, dir); err != nil {
			return err
		}
	}
	return x.stamp(ctx, tx)
}

func (x *azureReplayIndex) reconcileDirectory(ctx context.Context, tx *sql.Tx, stream, dir string) error {
	known := map[string]indexedReplayFile{}
	rows, err := tx.QueryContext(ctx, `SELECT name,bytes,modified,created,records FROM files WHERE stream=?`, stream)
	if err != nil {
		return err
	}
	for rows.Next() {
		f := indexedReplayFile{stream: stream}
		if err = rows.Scan(&f.name, &f.bytes, &f.modified, &f.created, &f.records); err != nil {
			_ = rows.Close()
			return err
		}
		known[f.name] = f
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		old, exists := known[entry.Name()]
		delete(known, entry.Name())
		if exists && old.bytes == info.Size() && old.modified == info.ModTime().UnixNano() {
			continue
		}
		var created time.Time
		var count int
		var readErr error
		if entry.Type()&os.ModeSymlink != 0 {
			readErr = errors.New("symlink replay file")
		} else {
			created, count, readErr = readAzureReplayMetadata(filepath.Join(dir, entry.Name()))
		}
		createdNano := created.UnixNano()
		if readErr != nil {
			createdNano, count = 0, 0
		} // retain poison for accounted removal by the worker
		f := indexedReplayFile{stream: stream, name: entry.Name(), bytes: info.Size(), modified: info.ModTime().UnixNano(), created: createdNano, records: count}
		if err = x.put(ctx, tx, f); err != nil {
			return err
		}
	}
	for name := range known {
		if _, err = tx.ExecContext(ctx, `DELETE FROM files WHERE stream=? AND name=?`, stream, name); err != nil {
			return err
		}
	}
	return nil
}

func (x *azureReplayIndex) put(ctx context.Context, tx *sql.Tx, f indexedReplayFile) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO files(stream,name,bytes,modified,created,records) VALUES(?,?,?,?,?,?)
 ON CONFLICT(stream,name) DO UPDATE SET bytes=excluded.bytes,modified=excluded.modified,created=excluded.created,records=excluded.records`, f.stream, f.name, f.bytes, f.modified, f.created, f.records)
	return err
}

func (x *azureReplayIndex) remove(ctx context.Context, tx *sql.Tx, f indexedReplayFile) error {
	path, err := x.path(f)
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM files WHERE stream=? AND name=?`, f.stream, f.name)
	return err
}

func replayIndexStats(ctx context.Context, db *sql.DB, stream string, now time.Time) (AzureReplayStats, error) {
	var result AzureReplayStats
	where := ""
	var args []any
	if stream != "*" {
		where = " WHERE stream=?"
		args = []any{stream}
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(records),0),COALESCE(SUM(bytes),0),COALESCE(SUM(files),0) FROM totals`+where, args...).Scan(&result.PendingRecords, &result.PendingBytes, &result.PendingFiles); err != nil {
		return result, err
	}
	var oldest sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(created) FROM files`+where, args...).Scan(&oldest); err != nil {
		return result, err
	}
	if oldest.Valid {
		result.OldestPendingAge = max(time.Duration(0), now.Sub(time.Unix(0, oldest.Int64)))
	}
	result.AccountingReady = true
	return result, nil
}

func (x *azureReplayIndex) stats(ctx context.Context, stream string, now time.Time) (AzureReplayStats, error) {
	if err := x.wait(ctx); err != nil {
		return AzureReplayStats{}, fmt.Errorf("replay accounting unavailable: %w", err)
	}
	return replayIndexStats(ctx, x.db, stream, now)
}

// A live root uses its shared connection; an external health command only
// opens an existing manifest read-only. No scan or new exporter is started.
func inspectReplayIndex(root string) (AzureReplayStats, bool) {
	root, err := filepath.Abs(root)
	if err != nil {
		return AzureReplayStats{}, false
	}
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		root = canonical
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	azureReplayIndexes.Lock()
	x := azureReplayIndexes.roots[root]
	azureReplayIndexes.Unlock()
	if x != nil {
		stats, _ := x.stats(ctx, "*", time.Now())
		return stats, true
	}
	path := filepath.Join(root, azureReplayIndexName)
	if _, err = os.Stat(path); err != nil {
		return AzureReplayStats{}, false
	}
	db, err := sql.Open("sqlite", sqliteuri.File(path)+"?mode=ro&_pragma=busy_timeout(50)")
	if err != nil {
		return AzureReplayStats{}, true
	}
	defer func() { _ = db.Close() }()
	var version int
	if err = db.QueryRowContext(ctx, "SELECT version FROM schema_meta").Scan(&version); err != nil || version != 1 {
		return AzureReplayStats{}, true
	}
	stats, _ := replayIndexStats(ctx, db, "*", time.Now())
	return stats, true
}
