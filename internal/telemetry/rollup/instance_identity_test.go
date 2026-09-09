package rollup

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestInstanceIdentityMigrationPreservesLegacyUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	legacy, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if _, err := legacy.Exec(`CREATE TABLE schema_meta (version INTEGER NOT NULL); INSERT INTO schema_meta VALUES (23)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:23] {
		if _, err := legacy.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO runs (run_id, workflow, workflow_version, gaggle, started_at)
		VALUES ('legacy', 'landing', 1, 'web', '2026-09-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var identity sql.NullString
	if err := db.sql.QueryRow(`SELECT instance_id FROM runs WHERE run_id='legacy'`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if identity.Valid {
		t.Fatalf("migration invented legacy provenance: %+v", identity)
	}
	runs, err := db.Runs(context.Background())
	if err != nil || len(runs) != 1 || runs[0].InstanceID != "" {
		t.Fatalf("legacy query = %+v, %v", runs, err)
	}
}

func TestIngestInstanceIdentityIsValidatedAndRetainedPerRun(t *testing.T) {
	for _, identity := range []string{"", "invalid", strings.Repeat("0", 32), strings.Repeat("A", 32), strings.Repeat("a", 32)} {
		t.Run("identity-"+identity, func(t *testing.T) {
			root := t.TempDir()
			run, err := journal.Create(filepath.Join(root, "runs"), journal.RunIdentity{
				RunID: fixtureRunID, Workflow: "landing", Gaggle: "web", InstanceID: identity,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			db := openTestDB(t, root)
			for range 2 {
				if err := db.IngestRun(context.Background(), run.Dir()); err != nil {
					t.Fatal(err)
				}
			}
			want := ""
			if identity == strings.Repeat("a", 32) {
				want = identity
			}
			runs, err := db.Runs(context.Background())
			if err != nil || len(runs) != 1 || runs[0].InstanceID != want {
				t.Fatalf("reingested runs = %+v, %v, want identity %q", runs, err, want)
			}
			if err := db.DeleteRun(context.Background(), fixtureRunID); err != nil {
				t.Fatal(err)
			}
			runs, err = db.Runs(context.Background())
			if err != nil || len(runs) != 0 {
				t.Fatalf("retention left instance provenance behind: %+v, %v", runs, err)
			}
		})
	}
}
