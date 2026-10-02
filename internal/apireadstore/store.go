// Package apireadstore persists conditional HTTP responses with indexed,
// incremental updates. It is a disposable cache, never an authority.
package apireadstore

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // Registers the cache database driver.

	"github.com/goobers/goobers/internal/sqliteuri"
)

// FileName identifies the disposable indexed response cache.
const FileName = "api-read-cache-v2.db"

// Entry separates small response metadata from shared content-addressed bodies.
type Entry struct {
	Key      string
	Metadata []byte
	Body     []byte
	Stored   int64
	Expires  int64
	Snapshot string
}

// Store has no background goroutines or persistent handles. Callers bound lock
// acquisition before opening it and close it after each operation.
type Store struct {
	db                   *sql.DB
	maxEntries, maxBytes int
}

// Open creates a disposable cache. The former JSON cache is intentionally cold:
// cached responses need no migration or correctness-critical recovery.
func Open(dir string, maxEntries, maxBytes int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	// Provider responses may contain private repository data. Create the
	// database with the same owner-only mode as the former atomic JSON files.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteuri.File(path)+"?_pragma=busy_timeout(1000)&_pragma=journal_mode(DELETE)&_pragma=synchronous(NORMAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, maxEntries: maxEntries, maxBytes: maxBytes}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS bodies (ref TEXT PRIMARY KEY, body BLOB NOT NULL, refs INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS entries (key TEXT PRIMARY KEY, metadata BLOB NOT NULL, ref TEXT NOT NULL, stored INTEGER NOT NULL, expires INTEGER NOT NULL, snapshot TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS entries_eviction ON entries(stored, (snapshot = ''), key DESC);
CREATE INDEX IF NOT EXISTS entries_expiry ON entries(expires);
CREATE INDEX IF NOT EXISTS entries_snapshot ON entries(snapshot);
CREATE TABLE IF NOT EXISTS totals (id INTEGER PRIMARY KEY CHECK(id=1), entries INTEGER NOT NULL, bytes INTEGER NOT NULL);
INSERT OR IGNORE INTO totals VALUES (1,0,0);
CREATE TRIGGER IF NOT EXISTS body_added AFTER INSERT ON bodies BEGIN UPDATE totals SET bytes=bytes+length(NEW.body) WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS body_removed AFTER DELETE ON bodies BEGIN UPDATE totals SET bytes=bytes-length(OLD.body) WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS entry_added AFTER INSERT ON entries BEGIN UPDATE bodies SET refs=refs+1 WHERE ref=NEW.ref; UPDATE totals SET entries=entries+1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS entry_removed AFTER DELETE ON entries BEGIN UPDATE bodies SET refs=refs-1 WHERE ref=OLD.ref; DELETE FROM bodies WHERE ref=OLD.ref AND refs=0; UPDATE totals SET entries=entries-1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS entry_replaced AFTER UPDATE OF ref ON entries WHEN OLD.ref != NEW.ref BEGIN UPDATE bodies SET refs=refs+1 WHERE ref=NEW.ref; UPDATE bodies SET refs=refs-1 WHERE ref=OLD.ref; DELETE FROM bodies WHERE ref=OLD.ref AND refs=0; END;
`

// Get reads only the requested response, including its expiry and integrity.
func (s *Store) Get(key string, now time.Time) (Entry, bool, error) {
	e := Entry{Key: key}
	var ref string
	err := s.db.QueryRow(`SELECT e.metadata,b.body,e.stored,e.expires,e.snapshot,e.ref FROM entries e JOIN bodies b ON b.ref=e.ref WHERE e.key=? AND e.expires>=?`, key, now.Unix()).Scan(&e.Metadata, &e.Body, &e.Stored, &e.Expires, &e.Snapshot, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	if bodyRef(e.Body) != ref {
		return Entry{}, false, nil
	}
	return e, true, nil
}

func bodyRef(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }

// Put atomically stores one response or a base response and its snapshot alias.
// Unchanged responses do not rewrite metadata or bodies. Eviction uses indexed
// oldest-entry selection and incremental reference counts, never directory scans
// or full-cache serialization. SQLite reuses free pages; VACUUM is deliberately
// absent from the request path.
func (s *Store) Put(entries ...Entry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range entries {
		if err := s.put(tx, e); err != nil {
			return err
		}
	}
	if err := s.evict(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) put(tx *sql.Tx, e Entry) error {
	if len(e.Body) > s.maxBytes {
		_, err := tx.Exec(`DELETE FROM entries WHERE key=?`, e.Key)
		return err
	}
	if e.Body == nil {
		e.Body = []byte{}
	}
	ref := bodyRef(e.Body)
	var oldMetadata []byte
	var oldRef, oldSnapshot string
	var stored, expires int64
	err := tx.QueryRow(`SELECT metadata,ref,stored,expires,snapshot FROM entries WHERE key=?`, e.Key).Scan(&oldMetadata, &oldRef, &stored, &expires, &oldSnapshot)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Preserve freshness on real refreshes, but coalesce repeated saves in the
	// same second (including aliases) when every persisted field is unchanged.
	if err == nil && oldRef == ref && oldSnapshot == e.Snapshot && stored == e.Stored && expires == e.Expires && bytes.Equal(oldMetadata, e.Metadata) {
		return nil
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO bodies(ref,body) VALUES(?,?)`, ref, e.Body); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO entries(key,metadata,ref,stored,expires,snapshot) VALUES(?,?,?,?,?,?) ON CONFLICT(key) DO UPDATE SET metadata=excluded.metadata,ref=excluded.ref,stored=excluded.stored,expires=excluded.expires,snapshot=excluded.snapshot`, e.Key, e.Metadata, ref, e.Stored, e.Expires, e.Snapshot)
	return err
}

func (s *Store) evict(tx *sql.Tx) error {
	for {
		var count, size int
		if err := tx.QueryRow(`SELECT entries,bytes FROM totals WHERE id=1`).Scan(&count, &size); err != nil {
			return err
		}
		if count <= s.maxEntries && size <= s.maxBytes {
			return nil
		}
		if _, err := tx.Exec(`DELETE FROM entries WHERE key=(SELECT key FROM entries ORDER BY stored,(snapshot=''),key DESC LIMIT 1)`); err != nil {
			return err
		}
	}
}

// InvalidateSnapshot drops only the named evaluation's aliases. Body reclamation
// happens only when its last reference disappears, using indexed lookups.
func (s *Store) InvalidateSnapshot(snapshot string) error {
	_, err := s.db.Exec(`DELETE FROM entries WHERE snapshot=?`, snapshot)
	return err
}
