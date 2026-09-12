// Package cancelreceipt preserves cancellation identity and outcomes across
// daemon restarts. An unfinished receipt is uncertain, never permission to
// repeat a side effect whose previous outcome cannot be proved.
package cancelreceipt

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/sqliteschema"
	"github.com/goobers/goobers/internal/sqliteuri"

	_ "modernc.org/sqlite" // Registers the durable receipt driver.
)

// Retention limits bound durable receipt storage without evicting uncertainty.
const (
	MaxRecords = 10000
	MaxBytes   = 16 << 10
	Retention  = 7 * 24 * time.Hour
)

// Admission and completion errors are distinct from storage failures.
var (
	ErrConflict   = errors.New("cancelreceipt: key belongs to another request")
	ErrFull       = errors.New("cancelreceipt: receipt capacity exhausted")
	ErrTransition = errors.New("cancelreceipt: invalid completion")
)

// Receipt distinguishes a recorded result from an unfinished attempt.
type Receipt struct {
	Result   []byte
	Complete bool
}

// Store uses SQLite transactions to arbitrate independent daemon connections.
type Store struct{ db *sql.DB }

var migrations = []string{`CREATE TABLE IF NOT EXISTS cancellations (
	key TEXT PRIMARY KEY NOT NULL CHECK(length(CAST(key AS BLOB)) BETWEEN 1 AND 200),
	actor TEXT NOT NULL CHECK(length(CAST(actor AS BLOB)) <= 1024),
	payload BLOB NOT NULL CHECK(length(payload) BETWEEN 1 AND 16384),
	result BLOB CHECK(length(result) BETWEEN 1 AND 16384),
	created_ns INTEGER NOT NULL, finished_ns INTEGER,
	CHECK ((result IS NULL) = (finished_ns IS NULL))
)`}

// Open requires a private regular database under a daemon-owned directory.
// FULL synchronization makes both reservation and completion durable on return.
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("cancelreceipt: absolute path required")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		err = f.Close()
	} else if errors.Is(err, os.ErrExist) {
		var info os.FileInfo
		info, err = os.Lstat(path)
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("cancelreceipt: database must be regular")
		}
	}
	if err != nil {
		return nil, err
	}
	uri := sqliteuri.File(path)
	db, err := sql.Open("sqlite", uri+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)&_pragma=max_page_count(65536)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	err = sqliteschema.Migrate(context.Background(), db, "cancelreceipt", migrations)
	if err == nil {
		err = durability.SyncDir(filepath.Dir(path))
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the receipt database connection.
func (s *Store) Close() error { return s.db.Close() }

// Begin atomically binds a key to the actor and exact payload. Only fresh=true
// authorizes execution. Unfinished reservations are never aged out or evicted.
func (s *Store) Begin(ctx context.Context, key, actor string, payload []byte, now time.Time) (receipt Receipt, fresh bool, err error) {
	if !validKey(key) || len(actor) > 1024 || len(payload) == 0 || len(payload) > MaxBytes || now.IsZero() {
		return Receipt{}, false, errors.New("cancelreceipt: invalid request")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var savedActor string
	var savedPayload, result []byte
	err = tx.QueryRowContext(ctx, "SELECT actor,payload,result FROM cancellations WHERE key=?", key).Scan(&savedActor, &savedPayload, &result)
	if err == nil {
		if actor != savedActor || string(payload) != string(savedPayload) {
			return Receipt{}, false, ErrConflict
		}
		return Receipt{Result: result, Complete: result != nil}, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM cancellations WHERE finished_ns < ?", now.Add(-Retention).UnixNano()); err != nil {
		return Receipt{}, false, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM cancellations").Scan(&count); err != nil {
		return Receipt{}, false, err
	}
	if count >= MaxRecords {
		return Receipt{}, false, ErrFull
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO cancellations(key,actor,payload,created_ns) VALUES(?,?,?,?)", key, actor, payload, now.UnixNano()); err != nil {
		return Receipt{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Receipt{}, false, err
	}
	return Receipt{}, true, nil
}

func validKey(key string) bool {
	if key == "" || len(key) > 200 || strings.TrimSpace(key) != key {
		return false
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Finish publishes an outcome once. A failed completion leaves an uncertain
// reservation, so a caller must not acknowledge an unrecorded result.
func (s *Store) Finish(ctx context.Context, key string, result []byte, now time.Time) error {
	if len(result) == 0 || len(result) > MaxBytes || now.IsZero() {
		return ErrTransition
	}
	res, err := s.db.ExecContext(ctx, "UPDATE cancellations SET result=?,finished_ns=? WHERE key=? AND result IS NULL AND created_ns<=?", result, now.UnixNano(), key, now.UnixNano())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTransition
	}
	return nil
}
