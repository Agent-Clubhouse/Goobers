//go:build cloudintegration

package migrate

import (
	"context"
	"slices"
	"testing"

	cloudschema "github.com/goobers/goobers/internal/cloud/schema"
)

// predecessorReleaseSQL is the previous binary's claim release, verbatim from
// internal/cloud/claims/history.go releaseToHistory before 0035. Its
// RETURNING * needs SELECT on every column of control.claim, including any a
// later migration adds.
const predecessorReleaseSQL = `
		WITH moved AS (
			DELETE FROM control.claim
			WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
			  AND run_id=$5 AND owner_epoch<=$6 AND ($8='' OR owner_token=$8)
			  AND (NOT $9 OR expires_at<=clock_timestamp())
			RETURNING *, clock_timestamp() AS released
		), incarnation AS (
			INSERT INTO control.claim_history
				(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
				 claimed_at,renewed_at,expires_at,released_at,release_reason,verification)
			SELECT instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,$6,
			       claimed_at,renewed_at,expires_at,released,$7,verification
			FROM moved
		)
		INSERT INTO control.claim_run_history (instance_id,run_id,gaggle,provider,external_id,workflow,owner_token,owner_epoch,
	claimed_at,renewed_at,expires_at,released_at,release_reason,verification)
		SELECT instance_id,run_id,gaggle,provider,external_id,workflow,owner_token,$6,
		       claimed_at,renewed_at,expires_at,released,$7,verification
		FROM moved
	ON CONFLICT (instance_id,run_id,gaggle,provider,external_id) DO UPDATE
	SET workflow=excluded.workflow, owner_token=excluded.owner_token, owner_epoch=excluded.owner_epoch,
	    claimed_at=excluded.claimed_at, renewed_at=excluded.renewed_at, expires_at=excluded.expires_at,
	    released_at=excluded.released_at, release_reason=excluded.release_reason,
	    verification=excluded.verification`

// writePathsBefore0035 is the current inventory without 0035's column.
func writePathsBefore0035() (cloudschema.Inventory, error) {
	inventory, err := cloudschema.LoadWritePaths()
	if err != nil {
		return inventory, err
	}
	added := func(column string) bool { return column == "release_pending_at" }
	for i := range inventory.Tables {
		table := &inventory.Tables[i]
		for j := range table.Readers {
			table.Readers[j].Columns = slices.DeleteFunc(slices.Clone(table.Readers[j].Columns), added)
		}
		for j := range table.Writers {
			table.Writers[j].Insert = slices.DeleteFunc(slices.Clone(table.Writers[j].Insert), added)
			table.Writers[j].Update = slices.DeleteFunc(slices.Clone(table.Writers[j].Update), added)
		}
	}
	return inventory, nil
}

// TestPredecessorReleaseSurvives0035BeforeTheGrantReconcile: migrations commit
// one leased step at a time and the grant reconcile runs after the last one, so
// between 0035 committing and the reconcile the previous binary is still
// serving. Its release must not fail for want of SELECT on the column 0035
// adds.
func TestPredecessorReleaseSurvives0035BeforeTheGrantReconcile(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var before []Migration
	var m0035 Migration
	for _, migration := range catalog {
		switch {
		case migration.Version == 35:
			m0035 = migration
		case migration.Version < 35:
			before = append(before, migration)
		}
	}
	if m0035.Version != 35 {
		t.Fatal("catalog has no migration 0035")
	}
	runner := Runner{Direct: pool, Catalog: before, BackupRef: "test", InstanceID: "migration", Holder: "test", writePaths: writePathsBefore0035}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatalf("apply catalog before 0035: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO control.claim (instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,claimed_at,renewed_at,expires_at,verification)
		VALUES ('i','g','github','42','run-a','w','tok',1,now(),now(),now()+interval '1 hour','{}')`); err != nil {
		t.Fatal(err)
	}
	// 0035 alone, exactly as its leased step commits it, with the current
	// inventory: no reconcile follows.
	inventory, err := cloudschema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = applyOne(t.Context(), conn, m0035, "test", inventory)
	conn.Release()
	if err != nil {
		t.Fatalf("apply 0035: %v", err)
	}
	controller := controllerRolePool(t, pool)
	tx, err := controller.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	tag, err := tx.Exec(t.Context(), predecessorReleaseSQL, "i", "g", "github", "42", "run-a", int64(1), []byte(`{}`), "", false)
	if err != nil {
		t.Fatalf("previous binary's release between 0035 and the grant reconcile: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("previous binary's release wrote %d history rows, want 1", tag.RowsAffected())
	}
}
