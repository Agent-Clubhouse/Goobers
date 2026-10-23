//go:build cloudintegration

package schema_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/schema"
)

func TestPostgresColumnGrantsMatchWritePathInventory(t *testing.T) {
	pool, databaseURL := migratedPool(t)
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	assertPrivilegesMatch(t, pool, inventory)
	assertRuntimeRoleQueries(t, databaseURL)
}

// TestGrantReconcileConvergesOnReapply shows a re-apply removes a privilege
// granted outside the inventory, and a changed inventory converges without a
// new migration.
func TestGrantReconcileConvergesOnReapply(t *testing.T) {
	pool, _ := migratedPool(t)
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "GRANT UPDATE (holder) ON control.lease TO goobers_api; GRANT DELETE ON control.lease_history TO goobers_controller"); err != nil {
		t.Fatal(err)
	}
	apply(t, pool)
	assertPrivilegesMatch(t, pool, inventory)

	changed := inventory
	changed.Tables = append([]schema.Table(nil), inventory.Tables...)
	for i, table := range changed.Tables {
		if table.Name == "lease_history" {
			changed.Tables[i].Readers = append(append([]schema.Reader(nil), table.Readers...), schema.Reader{Role: "goobers_api", Columns: []string{"epoch"}})
		}
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ReconcileGrants(t.Context(), tx, changed); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPrivilegesMatch(t, pool, changed)
}

func TestMigrateRefusesWithoutProvisionedRoles(t *testing.T) {
	pool, _ := migratedPool(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Roles are cluster-wide; rename one inside this transaction only.
	if _, err := tx.Exec(t.Context(), "ALTER ROLE goobers_api RENAME TO goobers_api_hidden"); err != nil {
		t.Fatal(err)
	}
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ReconcileGrants(t.Context(), tx, inventory); !errors.Is(err, schema.ErrRoleMissing) {
		t.Fatalf("reconcile without goobers_api = %v, want ErrRoleMissing", err)
	}
}

func migratedPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := "schema_" + strings.ToLower(t.Name())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	pgtest.EnsureRoles(t, pool)
	apply(t, pool)
	databaseURLForTest, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseURLForTest.Path = "/" + name
	return pool, databaseURLForTest.String()
}

func apply(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := applyErr(t, pool); err != nil {
		t.Fatal(err)
	}
}

func applyErr(t *testing.T, pool *pgxpool.Pool) error {
	t.Helper()
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	return (migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "grant-test",
		InstanceID: "grant-test", Holder: "grant-test",
	}).Apply(t.Context())
}

func assertPrivilegesMatch(t *testing.T, pool *pgxpool.Pool, inventory schema.Inventory) {
	t.Helper()
	expected := make(map[string]bool)
	for _, table := range inventory.Tables {
		if !table.Provisioned {
			continue
		}
		for _, reader := range table.Readers {
			for _, column := range reader.Columns {
				expected[privilegeKey(reader.Role, table.Schema, table.Name, "SELECT", column)] = true
			}
		}
		for _, writer := range table.Writers {
			for _, column := range writer.Insert {
				expected[privilegeKey(writer.Role, table.Schema, table.Name, "INSERT", column)] = true
			}
			for _, column := range writer.Update {
				expected[privilegeKey(writer.Role, table.Schema, table.Name, "UPDATE", column)] = true
			}
			if writer.Delete {
				expected[privilegeKey(writer.Role, table.Schema, table.Name, "DELETE", "*")] = true
			}
		}
	}
	actual := loadWritePrivileges(t, pool)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("database privileges differ from inventory\nactual: %#v\nwant:   %#v", actual, expected)
	}
	// ACL rows miss inherited and owner privileges; compare what the roles
	// can effectively do as well.
	effective, err := schema.EffectivePrivileges(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if want := inventory.ExpectedPrivileges(); !reflect.DeepEqual(effective, want) {
		t.Fatalf("effective privileges differ from inventory\nactual: %v\nwant:   %v", effective, want)
	}
}

func assertRuntimeRoleQueries(t *testing.T, databaseURL string) {
	t.Helper()
	controller := rolePool(t, databaseURL, "goobers_controller")
	defer controller.Close()
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: controller, InstanceID: "grant-role-test", Name: "control", Holder: "controller",
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatalf("controller lease acquire: %v", err)
	}
	if err := manager.Renew(t.Context(), handle); err != nil {
		t.Fatalf("controller lease renew: %v", err)
	}
	if err := manager.WithFence(t.Context(), handle, func(context.Context, pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("controller fenced read: %v", err)
	}
	if err := manager.Release(t.Context(), handle); err != nil {
		t.Fatalf("controller lease release: %v", err)
	}

	api := rolePool(t, databaseURL, "goobers_api")
	defer api.Close()
	if _, err := migrate.AppliedMigrations(t.Context(), api); err != nil {
		t.Fatalf("API schema compatibility read: %v", err)
	}
}

func rolePool(t *testing.T, databaseURL, role string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func loadWritePrivileges(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT grantee, table_schema, table_name, privilege_type, column_name
		FROM information_schema.column_privileges
		WHERE table_schema IN ('control', 'journal', 'readmodel')
		  AND grantee IN ('goobers_api', 'goobers_controller')
		  AND privilege_type IN ('SELECT', 'INSERT', 'UPDATE')`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actual := make(map[string]bool)
	for rows.Next() {
		var role, schemaName, table, operation, column string
		if err := rows.Scan(&role, &schemaName, &table, &operation, &column); err != nil {
			t.Fatal(err)
		}
		actual[privilegeKey(role, schemaName, table, operation, column)] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	deleteRows, err := pool.Query(t.Context(), `
		SELECT grantee, table_schema, table_name
		FROM information_schema.table_privileges
		WHERE table_schema IN ('control', 'journal', 'readmodel')
		  AND grantee IN ('goobers_api', 'goobers_controller')
		  AND privilege_type='DELETE'`)
	if err != nil {
		t.Fatal(err)
	}
	defer deleteRows.Close()
	for deleteRows.Next() {
		var role, schemaName, table string
		if err := deleteRows.Scan(&role, &schemaName, &table); err != nil {
			t.Fatal(err)
		}
		actual[privilegeKey(role, schemaName, table, "DELETE", "*")] = true
	}
	if err := deleteRows.Err(); err != nil {
		t.Fatal(err)
	}
	return actual
}

func privilegeKey(role, schemaName, table, operation, column string) string {
	return role + "|" + schemaName + "." + table + "|" + operation + "|" + column
}

// The reviewer-3 probes below (R-1..R-4) are kept as regression tests. A
// reconcile that cannot make effective privileges equal the inventory must
// refuse the apply rather than leave extra privileges behind.

func TestReconcileRefusesUninventoriedSchemaObjects(t *testing.T) {
	pool, _ := migratedPool(t)
	if _, err := pool.Exec(t.Context(), `
		CREATE SCHEMA journal;
		CREATE TABLE journal.run (run_id text PRIMARY KEY, body text);
		GRANT USAGE ON SCHEMA journal TO goobers_api;
		GRANT ALL ON journal.run TO goobers_api`); err != nil {
		t.Fatal(err)
	}
	if err := applyErr(t, pool); err == nil || !strings.Contains(err.Error(), "journal.run has no write-path inventory entry") {
		t.Fatalf("apply with an uninventoried journal.run = %v", err)
	}
}

func TestReconcileRefusesInheritedPrivileges(t *testing.T) {
	pool, _ := migratedPool(t)
	if _, err := pool.Exec(t.Context(), `GRANT pg_write_all_data TO goobers_api`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `REVOKE pg_write_all_data FROM goobers_api`) })
	if err := applyErr(t, pool); !errors.Is(err, schema.ErrRolePrivileged) {
		t.Fatalf("apply with goobers_api in pg_write_all_data = %v, want ErrRolePrivileged", err)
	}
}

func TestReconcileRefusesRuntimeRoleOwnership(t *testing.T) {
	pool, _ := migratedPool(t)
	if _, err := pool.Exec(t.Context(), `ALTER TABLE control.lease_history OWNER TO goobers_controller`); err != nil {
		t.Fatal(err)
	}
	if err := applyErr(t, pool); !errors.Is(err, schema.ErrRolePrivileged) {
		t.Fatalf("apply with a controller-owned table = %v, want ErrRolePrivileged", err)
	}
}

func TestReconcileRevokesExtraGrants(t *testing.T) {
	pool, _ := migratedPool(t)
	if _, err := pool.Exec(t.Context(), `
		GRANT SELECT (holder) ON control.lease TO goobers_api;
		GRANT SELECT ON control.schema_migration TO PUBLIC;
		GRANT CREATE ON SCHEMA control TO goobers_api;
		GRANT USAGE, CREATE ON SCHEMA public TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	apply(t, pool)
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	assertPrivilegesMatch(t, pool, inventory)
}

// TestReconcileGrantsSequenceUsage: a sequence is reconciled like a table, so
// the role the inventory names can draw from it and no other can.
func TestReconcileGrantsSequenceUsage(t *testing.T) {
	pool, _ := migratedPool(t)
	if _, err := pool.Exec(t.Context(), `CREATE SEQUENCE control.reconcile_generation AS bigint`); err != nil {
		t.Fatal(err)
	}
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, pool, inventory); err == nil || !strings.Contains(err.Error(), "control.reconcile_generation has no provisioned write-path inventory entry") {
		t.Fatalf("reconcile with an uninventoried sequence = %v", err)
	}
	inventory.Sequences = append(inventory.Sequences, schema.Sequence{
		Schema: "control", Name: "reconcile_generation", Provisioned: true, Rationale: "test", Usage: []string{"goobers_controller"},
	})
	if err := reconcile(t, pool, inventory); err != nil {
		t.Fatal(err)
	}
	for role, allowed := range map[string]bool{"goobers_controller": true, "goobers_api": false} {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+role+"; SELECT nextval('control.reconcile_generation')")
		_ = tx.Rollback(context.Background())
		if (err == nil) != allowed {
			t.Fatalf("%s nextval = %v, want allowed=%v", role, err, allowed)
		}
	}
}

func reconcile(t *testing.T, pool *pgxpool.Pool, inventory schema.Inventory) error {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := schema.ReconcileGrants(t.Context(), tx, inventory); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

// TestReconcileRefusesUndeclaredSecurityDefiner is reviewer-3's function probe:
// an out-of-band SECURITY DEFINER function that writes lease.epoch would let
// any role act with the owner's rights.
func TestReconcileRefusesUndeclaredSecurityDefiner(t *testing.T) {
	pool, _ := migratedPool(t)
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION control.bump_epoch() RETURNS void LANGUAGE sql SECURITY DEFINER
		AS 'UPDATE control.lease SET epoch = epoch + 1'`); err != nil {
		t.Fatal(err)
	}
	if err := applyErr(t, pool); err == nil || !strings.Contains(err.Error(), "control.bump_epoch() is SECURITY DEFINER") {
		t.Fatalf("apply with an undeclared SECURITY DEFINER function = %v", err)
	}
}

// TestReconcileRevokesFunctionExecute: functions default to PUBLIC EXECUTE;
// after the reconcile only the inventory's roles may execute them.
func TestReconcileRevokesFunctionExecute(t *testing.T) {
	pool, _ := migratedPool(t)
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION control.plain(n integer) RETURNS integer LANGUAGE sql AS 'SELECT n';
		CREATE FUNCTION control.listed(n integer) RETURNS integer LANGUAGE sql AS 'SELECT n';
		CREATE PROCEDURE control.chore() LANGUAGE sql AS 'SELECT 1';
		CREATE AGGREGATE control.total(integer) (SFUNC = int4pl, STYPE = integer)`); err != nil {
		t.Fatal(err)
	}
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	inventory.Functions = append(inventory.Functions, schema.Function{
		Schema: "control", Name: "listed", Arguments: "n integer", Rationale: "test", Execute: []string{"goobers_controller"},
	})
	if err := reconcile(t, pool, inventory); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		role, function string
		want           bool
	}{
		{"goobers_api", "control.plain(integer)", false},
		{"goobers_controller", "control.plain(integer)", false},
		{"goobers_controller", "control.listed(integer)", true},
		{"goobers_api", "control.listed(integer)", false},
		{"goobers_controller", "control.chore()", false},
		{"goobers_api", "control.total(integer)", false},
	} {
		var got bool
		if err := pool.QueryRow(t.Context(), "SELECT has_function_privilege($1, $2, 'EXECUTE')", check.role, check.function).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != check.want {
			t.Fatalf("%s EXECUTE %s = %v, want %v", check.role, check.function, got, check.want)
		}
	}
}
