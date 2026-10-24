//go:build cloudintegration

package schema_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/schema"
)

func grantExisting(t *testing.T, pool *pgxpool.Pool, inventory schema.Inventory) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := schema.GrantExisting(t.Context(), tx, inventory); err != nil {
		t.Fatalf("GrantExisting: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func liveSchemas(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT nspname FROM pg_namespace WHERE nspname NOT IN ('pg_catalog','information_schema') AND nspname NOT LIKE 'pg\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return schemas
}

// TestGrantExistingGrantsTheInventoryOnWhatExistsAndNothingMore: from runtime
// roles holding nothing, on a schema missing a table and a column the
// inventory names (the state part-way through the catalog), the additive pass
// succeeds, grants every inventory privilege on what exists, and grants
// nothing the inventory does not.
func TestGrantExistingGrantsTheInventoryOnWhatExistsAndNothingMore(t *testing.T) {
	pool, _ := migratedPool(t)
	for _, schemaName := range liveSchemas(t, pool) {
		id := pgx.Identifier{schemaName}.Sanitize()
		for _, statement := range []string{
			"REVOKE ALL ON ALL TABLES IN SCHEMA " + id + " FROM goobers_api, goobers_controller",
			"REVOKE ALL ON ALL SEQUENCES IN SCHEMA " + id + " FROM goobers_api, goobers_controller",
			"REVOKE ALL ON SCHEMA " + id + " FROM goobers_api, goobers_controller",
		} {
			if _, err := pool.Exec(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Not yet created: a whole table and one column on a table that exists.
	for _, statement := range []string{
		"DROP TABLE control.claim_run_history",
		"ALTER TABLE control.claim DROP COLUMN verification",
	} {
		if _, err := pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	grantExisting(t, pool, inventory)

	expected := inventory.Expand(liveSchemas(t, pool)).ExpectedPrivileges()
	effective, err := schema.EffectivePrivileges(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	missingObject := func(p schema.Privilege) bool {
		return (p.Schema == "control" && p.Object == "claim_run_history") ||
			(p.Schema == "control" && p.Object == "claim" && p.Column == "verification")
	}
	for p := range effective {
		if p.Operation == "EXECUTE" {
			continue // functions are the full reconcile's, not this pass's
		}
		if !expected[p] {
			t.Errorf("granted beyond the inventory: %s", p)
		}
	}
	for p := range expected {
		if p.Operation == "EXECUTE" || missingObject(p) {
			continue
		}
		if !effective[p] {
			t.Errorf("inventory privilege on an existing object not granted: %s", p)
		}
	}
}

// TestGrantExistingNeverRevokes: a privilege outside the inventory survives
// the additive pass; only the full reconcile removes it.
func TestGrantExistingNeverRevokes(t *testing.T) {
	pool, _ := migratedPool(t)
	if _, err := pool.Exec(t.Context(), "GRANT DELETE ON control.lease TO goobers_api"); err != nil {
		t.Fatal(err)
	}
	extra := schema.Privilege{Role: "goobers_api", Schema: "control", Object: "lease", Column: "*", Operation: "DELETE"}
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	if inventory.ExpectedPrivileges()[extra] {
		t.Fatal("precondition: the planted privilege must be outside the inventory")
	}
	before, err := schema.EffectivePrivileges(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if !before[extra] {
		t.Fatal("precondition: the planted privilege must be effective")
	}
	grantExisting(t, pool, inventory)
	after, err := schema.EffectivePrivileges(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for p := range before {
		if !after[p] {
			t.Errorf("additive pass revoked %s", p)
		}
	}
	if err := reconcile(t, pool, inventory); err != nil {
		t.Fatal(err)
	}
	if final, err := schema.EffectivePrivileges(t.Context(), pool); err != nil || final[extra] {
		t.Fatalf("the full reconcile must still remove %s (err %v)", extra, err)
	}
}
