//go:build cloudintegration

package migrate

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudschema "github.com/goobers/goobers/internal/cloud/schema"
)

// TestMigrationGrantsItsNewColumnBeforeTheReconcile: migrations commit one at
// a time and the grant reconcile runs only after the last one. A still-serving
// previous binary whose SQL reads every column (RETURNING *, SELECT *) would be
// refused a column a migration just added until then, so each migration grants
// the inventory's privileges on what exists in its own transaction.
func TestMigrationGrantsItsNewColumnBeforeTheReconcile(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (Runner{Direct: pool, Catalog: catalog, BackupRef: "test", InstanceID: "migration", Holder: "test"}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	last := catalog[len(catalog)-1]
	probe := func(version int, column string) Migration {
		m := last
		m.Version, m.Name = version, "probe"
		m.SQL = "ALTER TABLE control.claim ADD COLUMN " + column + " text"
		return m
	}
	inventory, err := cloudschema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	withColumn := func(column string) cloudschema.Inventory {
		extended := inventory
		extended.Tables = slices.Clone(inventory.Tables)
		for i, table := range extended.Tables {
			if table.Schema != "control" || table.Name != "claim" {
				continue
			}
			readers := slices.Clone(table.Readers)
			for j := range readers {
				if readers[j].Role == "goobers_controller" {
					readers[j].Columns = append(slices.Clone(readers[j].Columns), column)
				}
			}
			extended.Tables[i].Readers = readers
		}
		return extended
	}
	controller := controllerRolePool(t, pool)
	readAll := func() error {
		var n int
		return controller.QueryRow(t.Context(), "SELECT count(*) FROM (SELECT * FROM control.claim) AS c").Scan(&n)
	}
	apply := func(m Migration, inv cloudschema.Inventory) {
		t.Helper()
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if err := applyOne(t.Context(), conn, m, "test", inv); err != nil {
			t.Fatalf("apply probe migration: %v", err)
		}
	}

	// Without the column in the inventory the window is real: the previous
	// binary's read is refused until a reconcile.
	apply(probe(last.Version+1, "probe_ungranted"), inventory)
	var denied *pgconn.PgError
	if err := readAll(); !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatalf("an ungranted new column must be refused (42501), got %v", err)
	}
	if _, err := pool.Exec(t.Context(), "ALTER TABLE control.claim DROP COLUMN probe_ungranted"); err != nil {
		t.Fatal(err)
	}
	// With it, the migration's own transaction grants it: no reconcile ran.
	apply(probe(last.Version+2, "probe_granted"), withColumn("probe_granted"))
	if err := readAll(); err != nil {
		t.Fatalf("a column the inventory grants must be readable as soon as its migration commits: %v", err)
	}
}

func controllerRolePool(t *testing.T, base *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := base.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var role string
	if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("pool must run as goobers_controller, got %q (%v)", role, err)
	}
	return pool
}
