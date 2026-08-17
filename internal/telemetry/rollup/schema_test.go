package rollup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
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
	const wantDigest = "39ba7b282237cc12fb1fdf908afe3c0453cdb82da7d288b3d572aed6e8bf593d"
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
	} else if !strings.Contains(err.Error(), "newer than supported") {
		t.Errorf("error = %v; want a clear newer-schema refusal", err)
	}
}

// TestIngestRefusesUnknownRunSchema pins #2054 on the ingest side: a run.yaml
// reshaped by a future build must be refused, not silently ingested with
// zero-valued fields for whatever this build doesn't recognize. Mirrors
// internal/journal.Reader.Identity's same refusal for the exact reason
// mirror.go's package comment gives for keeping runIdentity un-imported: this
// package decodes run.yaml itself and so must apply the same schema check
// independently.
func TestIngestRefusesUnknownRunSchema(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	runDir := writeFixtureRun(t, runsDir, fixtureRunID, fixtureStart)

	b, err := os.ReadFile(filepath.Join(runDir, fileRunYAML))
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(b), "schema: "+runSchema, "schema: goobers.dev/journal/run/v2", 1)
	if tampered == string(b) {
		t.Fatal("test setup: schema line not found in run.yaml")
	}
	if err := os.WriteFile(filepath.Join(runDir, fileRunYAML), []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), runDir); err == nil {
		t.Fatal("IngestRun accepted a run.yaml with an unknown schema version instead of refusing it")
	} else if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error = %v; want a clear unsupported-schema refusal", err)
	}

	runs, err := db.Runs(context.Background())
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("ingest of a refused run left %d row(s) behind, want 0", len(runs))
	}
}
