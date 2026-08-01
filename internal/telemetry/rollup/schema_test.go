package rollup

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

// migrationPrefixDigest hashes an ordered slice of migration statements so a
// reordering or in-place edit of any one of them changes the result, even
// though nothing about the slice's length does.
func migrationPrefixDigest(prefix []string) string {
	h := sha256.New()
	for _, m := range prefix {
		h.Write([]byte(m))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestMigrationPrefixIsAppendOnly pins schema.go's "never edit a migration once
// released — append a new one" rule with something other than a comment (#2049).
// Only the newest migration is left out of the pinned prefix, so a legitimate
// append requires updating wantDigest — but reordering, editing, or inserting
// before it changes the digest of migrations that were never touched by this
// commit, which is exactly the class of mistake the comment alone cannot catch:
// every upgraded store silently stops applying the inserted DDL forever while
// fresh stores get it, the worst kind of schema divergence.
func TestMigrationPrefixIsAppendOnly(t *testing.T) {
	const wantDigest = "cff8ba8d9636b385a92bfb449d905c68d2a0e91694f99f077c503047673ed7c5"
	if got := migrationPrefixDigest(migrations[:len(migrations)-1]); got != wantDigest {
		t.Fatalf("migration prefix digest = %s, want %s\n"+
			"migrations must be append-only. If this commit only APPENDED a new\n"+
			"migration to the end of the list, update wantDigest to the value\n"+
			"above. If it did anything else to an existing entry, that is the\n"+
			"bug #2049 exists to catch.", got, wantDigest)
	}
}

// TestMigrateOnceRefusesANewerSchema mirrors internal/readmodel's existing
// newer-binary guard (#2049): without it, a version rollback (or an
// older-CLI-against-daemon-maintained-telemetry.db window, which the
// self-update supervisor makes routine) finds version > len(migrations), the
// migration loop simply never runs, and Open succeeds anyway — so the next
// IngestRun silently delete-then-inserts against a schema this build does not
// understand, with the version stamp still at the newer value forever.
func TestMigrateOnceRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.sql.Exec(`DELETE FROM schema_meta`); err != nil {
		t.Fatalf("clear schema_meta: %v", err)
	}
	if _, err := db.sql.Exec(`INSERT INTO schema_meta (version) VALUES (?)`, len(migrations)+5); err != nil {
		t.Fatalf("bump schema_meta: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("opened a store whose schema is newer than this build supports")
	} else if !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("error = %v; want a clear newer-schema refusal", err)
	}
}
