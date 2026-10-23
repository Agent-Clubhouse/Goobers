//go:build cloudintegration

package schema_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
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
	for _, role := range schema.RuntimeRoles {
		if _, err := pool.Exec(t.Context(), fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE %s NOLOGIN; END IF;
			EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`, role, role)); err != nil {
			t.Fatal(err)
		}
	}
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
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "grant-test",
		InstanceID: "grant-test", Holder: "grant-test",
	}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
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
