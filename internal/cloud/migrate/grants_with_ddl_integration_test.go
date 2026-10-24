//go:build cloudintegration

package migrate

import (
	"context"
	"errors"
	"slices"
	"strings"
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
// the inventory's privileges on what exists in its own transaction. The probe
// runs through Runner.Apply above leaseSchemaVersion, the leased path real
// migrations take, and the reconcile after it is made to fail: the grant must
// already be in place, and must hold, without it.
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
	inventory, err := cloudschema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	last := catalog[len(catalog)-1]
	if last.Version <= leaseSchemaVersion {
		t.Fatalf("precondition: the probe must run on the leased path (version > %d)", leaseSchemaVersion)
	}
	// withProbe returns the inventory granting goobers_controller the probe
	// columns on control.claim, plus a provisioned table that does not exist,
	// which makes the reconcile after the last migration fail.
	withProbe := func(columns ...string) func() (cloudschema.Inventory, error) {
		return func() (cloudschema.Inventory, error) {
			extended := inventory
			extended.Tables = slices.Clone(inventory.Tables)
			for i, table := range extended.Tables {
				if table.Schema != "control" || table.Name != "claim" {
					continue
				}
				readers := slices.Clone(table.Readers)
				for j := range readers {
					if readers[j].Role == "goobers_controller" {
						readers[j].Columns = append(slices.Clone(readers[j].Columns), columns...)
					}
				}
				extended.Tables[i].Readers = readers
			}
			extended.Tables = append(extended.Tables, cloudschema.Table{Schema: "control", Name: "probe_reconcile_fails", Provisioned: true})
			return extended, nil
		}
	}
	controller := controllerRolePool(t, pool)
	readAll := func() error {
		var n int
		return controller.QueryRow(t.Context(), "SELECT count(*) FROM (SELECT * FROM control.claim) AS c").Scan(&n)
	}
	applyProbe := func(version int, column string, granted ...string) {
		t.Helper()
		probe := last
		probe.Version, probe.Name = version, "probe"
		probe.SQL = "ALTER TABLE control.claim ADD COLUMN " + column + " text"
		runner := Runner{Direct: pool, Catalog: append(slices.Clone(catalog), probe), BackupRef: "test", InstanceID: "migration", Holder: "test", writePaths: withProbe(granted...)}
		if err := runner.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "probe_reconcile_fails") {
			t.Fatalf("precondition: the reconcile after the probe must fail, got %v", err)
		}
	}

	// A column the inventory does not grant stays refused: the window is real.
	applyProbe(last.Version+1, "probe_ungranted")
	var denied *pgconn.PgError
	if err := readAll(); !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatalf("an ungranted new column must be refused (42501), got %v", err)
	}
	if _, err := pool.Exec(t.Context(), "ALTER TABLE control.claim DROP COLUMN probe_ungranted"); err != nil {
		t.Fatal(err)
	}
	// A column the inventory grants is readable once its migration commits,
	// although the reconcile failed.
	applyProbe(last.Version+2, "probe_granted", "probe_granted")
	if err := readAll(); err != nil {
		t.Fatalf("a column the inventory grants must be readable as soon as its migration commits, reconcile or not: %v", err)
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
