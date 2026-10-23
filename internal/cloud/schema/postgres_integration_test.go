//go:build cloudintegration

package schema_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/schema"
)

func TestPostgresColumnGrantsMatchWritePathInventory(t *testing.T) {
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(t.Context(), "DROP SCHEMA IF EXISTS control CASCADE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS control CASCADE")
	})
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
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	expected := make(map[string]bool)
	for _, table := range inventory.Tables {
		if !table.Provisioned {
			continue
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
		t.Fatalf("database write privileges differ from inventory\nactual: %#v\nwant:   %#v", actual, expected)
	}
}

func loadWritePrivileges(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT grantee, table_schema, table_name, privilege_type, column_name
		FROM information_schema.column_privileges
		WHERE table_schema IN ('control', 'journal', 'readmodel')
		  AND grantee IN ('goobers_api', 'goobers_controller')
		  AND privilege_type IN ('INSERT', 'UPDATE')`,
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
