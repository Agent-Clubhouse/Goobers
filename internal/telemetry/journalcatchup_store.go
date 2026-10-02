package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
	"github.com/goobers/goobers/internal/sqliteuri"
)

// Unlike the rebuildable spool manifest, this database is a durable source
// cursor. Losing it may replay records, so journal wire IDs are deterministic.
// Bounds: a rolling replay-age window and at most 100,000 journal cursors.
const journalCursorSchema = `
CREATE TABLE IF NOT EXISTS enrollment(id INTEGER PRIMARY KEY CHECK(id=1), since INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS cursors(path TEXT PRIMARY KEY, identity TEXT NOT NULL, generation TEXT NOT NULL,
 offset INTEGER NOT NULL, seq INTEGER NOT NULL, touched INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS cursors_touched ON cursors(touched);`

var journalCursorMigrations = []string{
	journalCursorSchema,
	`ALTER TABLE cursors ADD COLUMN fingerprint TEXT NOT NULL DEFAULT '';`,
}

type journalCursor struct {
	identity, generation string
	offset               int64
	seq                  uint64
	fingerprint          string
}

func openJournalCursorStore(ctx context.Context, root string, since time.Time) (*sql.DB, time.Time, error) {
	// #6058: sqliteuri.File requires an absolute path; a relative spool root
	// would resolve to the filesystem root and the store could never open.
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, time.Time{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, time.Time{}, err
	}
	path := filepath.Join(root, ".journal-cursors.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, time.Time{}, err
	}
	err = f.Chmod(0o600)
	_ = f.Close()
	if err != nil {
		return nil, time.Time{}, err
	}
	db, err := sql.Open("sqlite", sqliteuri.File(path)+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(1000)&_txlock=immediate")
	if err != nil {
		return nil, time.Time{}, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*sql.DB, time.Time, error) { _ = db.Close(); return nil, time.Time{}, err }
	if err = sqliteschema.Migrate(ctx, db, "journal-export-cursors", journalCursorMigrations); err != nil {
		return fail(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO enrollment VALUES(1,?)", since.UnixNano()); err != nil {
		return fail(err)
	}
	var enrolled int64
	if err = db.QueryRowContext(ctx, "SELECT since FROM enrollment WHERE id=1").Scan(&enrolled); err != nil {
		return fail(err)
	}
	return db, time.Unix(0, enrolled), nil
}

func loadJournalCursor(ctx context.Context, db *sql.DB, path string) (journalCursor, error) {
	var cursor journalCursor
	err := db.QueryRowContext(ctx, "SELECT identity,generation,offset,seq,fingerprint FROM cursors WHERE path=?", path).
		Scan(&cursor.identity, &cursor.generation, &cursor.offset, &cursor.seq, &cursor.fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return cursor, err
}

func saveJournalCursor(ctx context.Context, db *sql.DB, path string, cursor journalCursor) error {
	_, err := db.ExecContext(ctx, `INSERT INTO cursors(path,identity,generation,offset,seq,touched,fingerprint) VALUES(?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET
 identity=excluded.identity,generation=excluded.generation,offset=excluded.offset,seq=excluded.seq,touched=excluded.touched,fingerprint=excluded.fingerprint`,
		path, cursor.identity, cursor.generation, cursor.offset, cursor.seq, time.Now().UnixNano(), cursor.fingerprint)
	return err
}

func pruneJournalCursors(ctx context.Context, db *sql.DB, cutoff time.Time) error {
	_, err := db.ExecContext(ctx, `DELETE FROM cursors WHERE touched<? OR path IN
 (SELECT path FROM cursors ORDER BY touched DESC LIMIT -1 OFFSET 100000)`, cutoff.UnixNano())
	return err
}
