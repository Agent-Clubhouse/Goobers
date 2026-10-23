package schema

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// RuntimeRoles are the application roles whose privileges the inventory
// defines exactly. Role provisioning (goobers-cloud bootstrap) creates them;
// migrations never do.
var RuntimeRoles = []string{"goobers_api", "goobers_controller"}

// ErrRoleMissing means role provisioning has not run on this database.
var ErrRoleMissing = errors.New("cloud runtime role is missing; run goobers-cloud bootstrap (role provisioning) before migrate")

// ReconcileGrants converges the runtime roles' privileges on every table in
// the inventory's schemas to exactly the inventory. It revokes everything and
// re-grants inside the caller's transaction, so no reader observes a partial
// set, and it fails when a table and the inventory disagree about whether the
// table exists. The migrate Job runs it after the versioned migrations.
func ReconcileGrants(ctx context.Context, tx pgx.Tx, inventory Inventory) error {
	if err := requireRoles(ctx, tx); err != nil {
		return err
	}
	schemas, err := existingTables(ctx, tx, inventory)
	if err != nil {
		return err
	}
	statements, err := reconcileStatements(inventory, schemas)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("reconcile cloud grants: %s: %w", statement, err)
		}
	}
	return nil
}

func requireRoles(ctx context.Context, tx pgx.Tx) error {
	var missing []string
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(array_agg(role ORDER BY role), '{}')
		FROM unnest($1::text[]) AS role
		WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role)`,
		RuntimeRoles).Scan(&missing); err != nil {
		return fmt.Errorf("look up cloud runtime roles: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrRoleMissing, strings.Join(missing, ", "))
	}
	return nil
}

// existingTables maps each inventory schema that exists to its tables.
func existingTables(ctx context.Context, tx pgx.Tx, inventory Inventory) (map[string][]string, error) {
	var names []string
	for _, table := range inventory.Tables {
		if !slices.Contains(names, table.Schema) {
			names = append(names, table.Schema)
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT n.nspname, coalesce(array_agg(c.relname ORDER BY c.relname) FILTER (WHERE c.oid IS NOT NULL), '{}')
		FROM pg_namespace AS n
		LEFT JOIN pg_class AS c ON c.relnamespace = n.oid AND c.relkind IN ('r', 'p')
		WHERE n.nspname = ANY($1)
		GROUP BY n.nspname`, names)
	if err != nil {
		return nil, fmt.Errorf("list cloud tables: %w", err)
	}
	schemas := make(map[string][]string)
	for rows.Next() {
		var schema string
		var tables []string
		if err := rows.Scan(&schema, &tables); err != nil {
			return nil, err
		}
		schemas[schema] = tables
	}
	return schemas, rows.Err()
}

func reconcileStatements(inventory Inventory, schemas map[string][]string) ([]string, error) {
	policies := make(map[string]Table, len(inventory.Tables))
	for _, table := range inventory.Tables {
		policies[table.Schema+"."+table.Name] = table
	}
	roles := strings.Join(RuntimeRoles, ", ")
	var statements []string
	schemaNames := make([]string, 0, len(schemas))
	for schema := range schemas {
		schemaNames = append(schemaNames, schema)
	}
	sort.Strings(schemaNames)
	for _, schema := range schemaNames {
		statements = append(statements,
			fmt.Sprintf("REVOKE ALL ON SCHEMA %s FROM PUBLIC, %s", schema, roles),
			fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", schema, roles))
		for _, name := range schemas[schema] {
			qualified := schema + "." + name
			policy, ok := policies[qualified]
			if !ok {
				return nil, fmt.Errorf("cloud table %s has no write-path inventory entry", qualified)
			}
			if !policy.Provisioned {
				return nil, fmt.Errorf("cloud table %s exists but the inventory marks it unprovisioned", qualified)
			}
			statements = append(statements, fmt.Sprintf("REVOKE ALL ON %s FROM PUBLIC, %s", qualified, roles))
			statements = append(statements, tableGrants(policy)...)
		}
	}
	for _, table := range inventory.Tables {
		if table.Provisioned && !slices.Contains(schemas[table.Schema], table.Name) {
			return nil, fmt.Errorf("cloud write-path inventory marks %s.%s provisioned but it does not exist", table.Schema, table.Name)
		}
	}
	return statements, nil
}

func tableGrants(table Table) []string {
	qualified := table.Schema + "." + table.Name
	var statements []string
	for _, reader := range table.Readers {
		statements = append(statements, fmt.Sprintf("GRANT SELECT (%s) ON %s TO %s", strings.Join(reader.Columns, ", "), qualified, reader.Role))
	}
	for _, writer := range table.Writers {
		if len(writer.Insert) > 0 {
			statements = append(statements, fmt.Sprintf("GRANT INSERT (%s) ON %s TO %s", strings.Join(writer.Insert, ", "), qualified, writer.Role))
		}
		if len(writer.Update) > 0 {
			statements = append(statements, fmt.Sprintf("GRANT UPDATE (%s) ON %s TO %s", strings.Join(writer.Update, ", "), qualified, writer.Role))
		}
		if writer.Delete {
			statements = append(statements, fmt.Sprintf("GRANT DELETE ON %s TO %s", qualified, writer.Role))
		}
	}
	return statements
}
