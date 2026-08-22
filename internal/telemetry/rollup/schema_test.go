package rollup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	const wantDigest = "2cf0a87dec0311f71cd7751e7afce4f9371cfef38fa0eaf16a489ad519678242"
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

func TestCICheckFailureMigrationBackfillsExistingRunJournals(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "gaggles", "example", "runs")
	seedCICheckFailureRun(t, runsDir, strings.Repeat("a", 32), "unit-tests", fixtureStart)
	seedCICheckFailureRun(t, runsDir, strings.Repeat("b", 32), "unit-tests", fixtureStart.Add(time.Hour))

	path := filepath.Join(tmp, "telemetry.db")
	legacy, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_meta (version INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create legacy schema metadata: %v", err)
	}
	for i := 0; i < 18; i++ {
		if _, err := legacy.Exec(migrations[i]); err != nil {
			t.Fatalf("apply legacy migration %d: %v", i+1, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO schema_meta (version) VALUES (18)`); err != nil {
		t.Fatalf("set legacy schema version: %v", err)
	}
	for i, runID := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		if _, err := legacy.Exec(`
			INSERT INTO runs (run_id, workflow, workflow_version, gaggle, started_at)
			VALUES (?, 'implementation', 1, 'example', ?)`,
			runID, fixtureStart.Add(time.Duration(i)*time.Hour).Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert legacy run %s: %v", runID, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade Open: %v", err)
	}
	defer func() { _ = upgraded.Close() }()
	findings, err := upgraded.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect after upgrade: %v", err)
	}
	for _, finding := range findings {
		if finding.Kind == FindingCICheckFailure && finding.Subject == "unit-tests" {
			if finding.Metrics["distinctRuns"] != 2 {
				t.Fatalf("distinct runs = %v, want 2", finding.Metrics["distinctRuns"])
			}
			return
		}
	}
	t.Fatalf("backfilled recurring CI failure not found: %+v", findings)
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
