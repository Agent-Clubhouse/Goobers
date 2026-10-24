//go:build cloudintegration

package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudschema "github.com/goobers/goobers/internal/cloud/schema"
)

// upgradeTo0022 applies the catalog without 0022 and 0035 (which alters the
// table 0022 creates), runs seed against the pre-0022 schema, then applies the
// full catalog.
func upgradeTo0022(t *testing.T, seed string) *pgxpool.Pool {
	t.Helper()
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var before []Migration
	for _, migration := range catalog {
		if migration.Version != 22 && migration.Version != 35 {
			before = append(before, migration)
		}
	}
	runner := Runner{Direct: pool, Catalog: before, BackupRef: "test", InstanceID: "migration", Holder: "test",
		writePaths: writePathsBefore0022}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatalf("apply catalog without 0022: %v", err)
	}
	if _, err := pool.Exec(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	runner.Catalog, runner.writePaths = catalog, nil
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatalf("apply 0022: %v", err)
	}
	return pool
}

// writePathsBefore0022 is the current inventory without what 0022 and 0035
// add: the claim_run_history table and the cleanup-custody column.
func writePathsBefore0022() (cloudschema.Inventory, error) {
	inventory, err := cloudschema.LoadWritePaths()
	if err != nil {
		return inventory, err
	}
	inventory.Tables = slices.DeleteFunc(inventory.Tables, func(table cloudschema.Table) bool {
		return table.Schema == "control" && table.Name == "claim_run_history"
	})
	later := func(column string) bool { return column == "release_pending_at" }
	for i := range inventory.Tables {
		table := &inventory.Tables[i]
		for j := range table.Readers {
			table.Readers[j].Columns = slices.DeleteFunc(slices.Clone(table.Readers[j].Columns), later)
		}
		for j := range table.Writers {
			table.Writers[j].Insert = slices.DeleteFunc(slices.Clone(table.Writers[j].Insert), later)
			table.Writers[j].Update = slices.DeleteFunc(slices.Clone(table.Writers[j].Update), later)
		}
	}
	return inventory, nil
}

const populatedBefore0022 = `
	INSERT INTO control.claim_history
		(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
		 claimed_at,renewed_at,expires_at,released_at,release_reason)
	VALUES
		('i','g','github','1','run-a','w','old',1,now()-interval '3 hours',now(),now(),now()-interval '2 hours','{"disposition":"failed"}'),
		('i','g','github','1','run-a','w','new',1,now()-interval '1 hour',now(),now(),now()-interval '30 minutes','{"disposition":"completed"}'),
		('i','g','github','2','run-a','w','gone',1,now()-interval '1 hour',now(),now(),now(),'{"disposition":"completed"}');
	INSERT INTO control.claim
		(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,claimed_at,renewed_at,expires_at)
	VALUES
		('i','g','github','2','run-a','w','live',1,now(),now(),now()+interval '1 hour'),
		('i','g','github','item','run-b','w','incarnation',1,now(),now(),now()+interval '1 hour')`

// TestClaimRunHistorySeedsOneRecordPerRunAndKey: 0022 seeds the per-run
// records from each run and key's newest released incarnation, superseded by
// the run's live lease, and leaves control.claim_history untouched.
func TestClaimRunHistorySeedsOneRecordPerRunAndKey(t *testing.T) {
	pool := upgradeTo0022(t, populatedBefore0022)
	rows, err := pool.Query(t.Context(), `
		SELECT external_id, owner_token, released_at IS NULL FROM control.claim_run_history
		WHERE instance_id='i' AND run_id='run-a' ORDER BY external_id`)
	if err != nil {
		t.Fatal(err)
	}
	type record struct {
		item, token string
		live        bool
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record, error) {
		var r record
		return r, row.Scan(&r.item, &r.token, &r.live)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []record{{"1", "new", false}, {"2", "live", true}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("run records after 0022 = %+v, want the newest released record and the live lease", got)
	}
	var incarnations int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM control.claim_history WHERE instance_id='i'`).Scan(&incarnations); err != nil || incarnations != 3 {
		t.Fatalf("claim_history rows = %d (%v), want the 3 released incarnations untouched", incarnations, err)
	}
}

// TestClaim0022KeepsPredecessorHistoryReaderAndWriter: 0022 is an expand
// that keeps min-compatible-binary 0.6.0, so the previous binary must still
// read and release against an upgraded, populated database, as the
// controller role. The statements are the previous claims.Store's, verbatim:
// HistorySnapshot (released_at scanned into time.Time) and ReleaseScoped
// (DELETE + append INSERT, no upsert).
func TestClaim0022KeepsPredecessorHistoryReaderAndWriter(t *testing.T) {
	pool := upgradeTo0022(t, populatedBefore0022)
	st, err := Status(t.Context(), pool, "0.7.0")
	if err != nil || !st.Compatible {
		t.Fatalf("premise: the previous binary is still admitted: %+v, %v", st, err)
	}
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), `SET ROLE goobers_controller`); err != nil {
		t.Fatal(err)
	}
	predecessorSnapshot := func(step string) int {
		t.Helper()
		rows, err := conn.Query(t.Context(), `SELECT external_id,gaggle,provider,run_id,workflow,
			claimed_at,renewed_at,expires_at,verification,released_at,release_reason
			FROM control.claim_history WHERE instance_id=$1 ORDER BY released_at DESC`, "i")
		if err != nil {
			t.Fatalf("%s: predecessor HistorySnapshot query: %v", step, err)
		}
		count := 0
		for rows.Next() {
			var item, gaggle, provider, run, workflow string
			var claimed, renewed, expires, released time.Time
			var verification, reason []byte
			if err := rows.Scan(&item, &gaggle, &provider, &run, &workflow, &claimed, &renewed, &expires,
				&verification, &released, &reason); err != nil {
				t.Fatalf("%s: predecessor HistorySnapshot scan: %v", step, err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(reason, &decoded); err != nil {
				t.Fatalf("%s: predecessor release reason decode: %v", step, err)
			}
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		return count
	}
	predecessorRelease := func(item, run string) error {
		tag, err := conn.Exec(t.Context(), `
			WITH moved AS (
				DELETE FROM control.claim
				WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
				  AND run_id=$5 AND owner_epoch=$6
				RETURNING *
			)
			INSERT INTO control.claim_history
				(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
				 claimed_at,renewed_at,expires_at,released_at,release_reason,verification)
			SELECT instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
			       claimed_at,renewed_at,expires_at,clock_timestamp(),$7,verification
			FROM moved`,
			"i", "g", "github", item, run, int64(1), []byte(`{"disposition":"completed"}`))
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("released nothing")
		}
		return err
	}

	if n := predecessorSnapshot("after upgrade"); n != 3 {
		t.Fatalf("predecessor snapshot after upgrade = %d rows, want 3", n)
	}
	// The current binary's release appends the predecessor-shaped incarnation
	// alongside the run record (claims.releaseToHistory); simulate it here.
	if _, err := conn.Exec(context.WithoutCancel(t.Context()), `
		WITH moved AS (
			DELETE FROM control.claim WHERE instance_id='i' AND external_id='2' RETURNING *, clock_timestamp() AS released
		), incarnation AS (
			INSERT INTO control.claim_history
				(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
				 claimed_at,renewed_at,expires_at,released_at,release_reason,verification)
			SELECT instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
			       claimed_at,renewed_at,expires_at,released,'{"disposition":"completed"}',verification FROM moved
		)
		INSERT INTO control.claim_run_history
			(instance_id,run_id,gaggle,provider,external_id,workflow,owner_token,owner_epoch,
			 claimed_at,renewed_at,expires_at,released_at,release_reason,verification)
		SELECT instance_id,run_id,gaggle,provider,external_id,workflow,owner_token,owner_epoch,
		       claimed_at,renewed_at,expires_at,released,'{"disposition":"completed"}',verification FROM moved
		ON CONFLICT (instance_id,run_id,gaggle,provider,external_id) DO UPDATE
		SET released_at=excluded.released_at, release_reason=excluded.release_reason`); err != nil {
		t.Fatalf("current-binary release: %v", err)
	}
	if err := predecessorRelease("item", "run-b"); err != nil {
		t.Fatalf("predecessor ReleaseScoped after upgrade: %v", err)
	}
	var remaining int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM control.claim WHERE instance_id='i'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("active claims after both releases = %d (%v), want 0", remaining, err)
	}
	if n := predecessorSnapshot("after mixed releases"); n != 5 {
		t.Fatalf("predecessor snapshot after mixed releases = %d rows, want 5", n)
	}
}
