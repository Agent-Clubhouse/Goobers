package schema

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// GrantExisting applies the WHOLE inventory of the binary running the migrate
// (binary N) to the objects that exist at this moment, granting only. It runs
// inside each migration's transaction, right after its DDL: the grant
// reconcile runs only after the last migration, and in between a previous
// binary still serving would be refused a column a migration just added (a
// RETURNING * or SELECT * needs SELECT on every column).
//
// Because it is N's whole inventory, not just the migration's new objects:
//   - widenings N makes to existing objects take effect at the first migration
//     of the run, and persist if the run then fails before the reconcile;
//   - narrowings (privileges N removes) do not happen here; they wait for the
//     reconcile, since this pass never revokes.
//
// It skips silently whatever the inventory names that does not exist yet,
// which is the expected state part-way through the catalog: a missing role,
// schema, table, column or sequence, a partition (governed by its root) and an
// object the inventory does not describe. ReconcileGrants after the last
// migration stays the authority that converges privileges exactly.
func GrantExisting(ctx context.Context, tx pgx.Tx, inventory Inventory) error {
	roles, err := existingRuntimeRoles(ctx, tx)
	if err != nil || len(roles) == 0 {
		return err
	}
	schemas, objects, err := listObjects(ctx, tx)
	if err != nil {
		return err
	}
	columns, err := existingColumns(ctx, tx)
	if err != nil {
		return err
	}
	inventory = inventory.Expand(schemas)
	statements := additiveStatements(inventory, roles, schemas, objects, columns)
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("grant cloud privileges on existing objects: %s: %w", statement, err)
		}
	}
	return nil
}

func existingRuntimeRoles(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname = ANY($1)`, RuntimeRoles)
	if err != nil {
		return nil, fmt.Errorf("look up cloud runtime roles: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("look up cloud runtime roles: %w", err)
	}
	roles := make(map[string]bool, len(names))
	for _, name := range names {
		roles[name] = true
	}
	return roles, nil
}

// existingColumns maps each relation ("schema.name") to its live columns.
func existingColumns(ctx context.Context, tx pgx.Tx) (map[string]map[string]bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT n.nspname, c.relname, a.attname
		FROM pg_attribute AS a
		JOIN pg_class AS c ON c.oid = a.attrelid
		JOIN pg_namespace AS n ON n.oid = c.relnamespace
		WHERE `+systemSchemas+` AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped`)
	if err != nil {
		return nil, fmt.Errorf("list cloud columns: %w", err)
	}
	type column struct{ schema, table, name string }
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (column, error) {
		var c column
		return c, row.Scan(&c.schema, &c.table, &c.name)
	})
	if err != nil {
		return nil, fmt.Errorf("list cloud columns: %w", err)
	}
	columns := make(map[string]map[string]bool)
	for _, c := range found {
		key := c.schema + "." + c.table
		if columns[key] == nil {
			columns[key] = make(map[string]bool)
		}
		columns[key][c.name] = true
	}
	return columns, nil
}

func additiveStatements(inventory Inventory, roles map[string]bool, schemas []string, objects []object, columns map[string]map[string]bool) []string {
	tables, sequences := inventory.tableIndex(), inventory.sequenceIndex()
	present := func(role string) bool { return roles[role] }
	var statements []string
	for _, schema := range schemas {
		if users := slices.DeleteFunc(inventory.schemaUsers(schema), func(role string) bool { return !present(role) }); len(users) > 0 {
			statements = append(statements, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", ident(schema), strings.Join(users, ", ")))
		}
	}
	for _, o := range objects {
		if o.root != "" {
			continue
		}
		name := ident(o.schema) + "." + ident(o.name)
		switch o.kind {
		case "S":
			policy, ok := sequences[o.qualified()]
			if !ok || !policy.Provisioned {
				continue
			}
			if users := slices.DeleteFunc(slices.Clone(policy.Usage), func(role string) bool { return !present(role) }); len(users) > 0 {
				statements = append(statements, fmt.Sprintf("GRANT USAGE ON SEQUENCE %s TO %s", name, strings.Join(users, ", ")))
			}
		case "r", "p":
			policy, ok := tables[o.qualified()]
			if !ok || !policy.Provisioned {
				continue
			}
			live := func(list []string) []string {
				return slices.DeleteFunc(slices.Clone(list), func(column string) bool { return !columns[o.qualified()][column] })
			}
			for _, reader := range policy.Readers {
				if cols := live(reader.Columns); present(reader.Role) && len(cols) > 0 {
					statements = append(statements, fmt.Sprintf("GRANT SELECT (%s) ON %s TO %s", strings.Join(cols, ", "), name, reader.Role))
				}
			}
			for _, writer := range policy.Writers {
				if !present(writer.Role) {
					continue
				}
				if cols := live(writer.Insert); len(cols) > 0 {
					statements = append(statements, fmt.Sprintf("GRANT INSERT (%s) ON %s TO %s", strings.Join(cols, ", "), name, writer.Role))
				}
				if cols := live(writer.Update); len(cols) > 0 {
					statements = append(statements, fmt.Sprintf("GRANT UPDATE (%s) ON %s TO %s", strings.Join(cols, ", "), name, writer.Role))
				}
				if writer.Delete {
					statements = append(statements, fmt.Sprintf("GRANT DELETE ON %s TO %s", name, writer.Role))
				}
			}
		}
	}
	return statements
}
