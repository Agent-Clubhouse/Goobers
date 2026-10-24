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

var (
	// ErrRoleMissing means role provisioning has not run on this database.
	ErrRoleMissing = errors.New("cloud runtime role is missing; run goobers-cloud bootstrap (role provisioning) before migrate")
	// ErrRolePrivileged means a runtime role holds privileges no grant
	// reconcile can remove: an attribute, a role membership, or ownership.
	// Roles are cluster-wide and owned by role provisioning, so migrate refuses
	// rather than altering them.
	ErrRolePrivileged = errors.New("cloud runtime role holds privileges outside the write-path inventory; fix role provisioning before migrate")
	// ErrPrivilegeMismatch means a runtime role's effective privileges differ
	// from the inventory after the reconcile.
	ErrPrivilegeMismatch = errors.New("cloud runtime role privileges differ from the write-path inventory")
)

// object is one relation in a non-system schema. Partitions carry their root.
type object struct {
	schema, name, kind, owner, root string
}

func (o object) qualified() string { return o.schema + "." + o.name }

// ReconcileGrants converges the runtime roles' privileges on every schema,
// table and sequence in the database (not only the schemas the inventory
// names) to exactly the inventory. It refuses runtime roles that inherit or
// own privileges, revokes everything and re-grants inside the caller's
// transaction, and then verifies the roles' effective privileges, so a grant
// through any path the reconcile did not anticipate fails the apply. It also
// fails when an object and the inventory disagree. The migrate Job runs it
// after the versioned migrations.
func ReconcileGrants(ctx context.Context, tx pgx.Tx, inventory Inventory) error {
	if err := requireRoles(ctx, tx); err != nil {
		return err
	}
	schemas, objects, err := listObjects(ctx, tx)
	if err != nil {
		return err
	}
	inventory = inventory.Expand(schemas)
	for _, o := range objects {
		if slices.Contains(RuntimeRoles, o.owner) {
			return fmt.Errorf("%w: %s owns %s", ErrRolePrivileged, o.owner, o.qualified())
		}
	}
	statements, err := reconcileStatements(inventory, schemas, objects)
	if err != nil {
		return err
	}
	functions, err := loadFunctions(ctx, tx)
	if err != nil {
		return err
	}
	functionGrants, err := functionStatements(inventory, functions)
	if err != nil {
		return err
	}
	statements = append(statements, functionGrants...)
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("reconcile cloud grants: %s: %w", statement, err)
		}
	}
	return VerifyEffectivePrivileges(ctx, tx, inventory)
}

func requireRoles(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT r.role, a.rolname IS NOT NULL,
		       coalesce(a.rolsuper OR a.rolbypassrls OR a.rolcreaterole OR a.rolcreatedb OR a.rolreplication, false),
		       EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member = a.oid)
		FROM unnest($1::text[]) AS r(role)
		LEFT JOIN pg_roles AS a ON a.rolname = r.role
		ORDER BY r.role`, RuntimeRoles)
	if err != nil {
		return fmt.Errorf("look up cloud runtime roles: %w", err)
	}
	type state struct {
		role                         string
		exists, attributes, inherits bool
	}
	states, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (state, error) {
		var s state
		return s, row.Scan(&s.role, &s.exists, &s.attributes, &s.inherits)
	})
	if err != nil {
		return fmt.Errorf("look up cloud runtime roles: %w", err)
	}
	var missing []string
	for _, s := range states {
		switch {
		case !s.exists:
			missing = append(missing, s.role)
		case s.attributes:
			return fmt.Errorf("%w: %s has a superuser, bypassrls, createrole, createdb or replication attribute", ErrRolePrivileged, s.role)
		case s.inherits:
			return fmt.Errorf("%w: %s is a member of another role", ErrRolePrivileged, s.role)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrRoleMissing, strings.Join(missing, ", "))
	}
	return nil
}

// systemSchemas excludes catalogs, TOAST and temporary schemas.
const systemSchemas = `n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'`

// listObjects returns every non-system schema and the tables, views and
// sequences in them, excluding objects that belong to an extension.
func listObjects(ctx context.Context, tx pgx.Tx) ([]string, []object, error) {
	rows, err := tx.Query(ctx, `SELECT n.nspname FROM pg_namespace AS n WHERE `+systemSchemas+` ORDER BY 1`)
	if err != nil {
		return nil, nil, fmt.Errorf("list cloud schemas: %w", err)
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, fmt.Errorf("list cloud schemas: %w", err)
	}
	rows, err = tx.Query(ctx, `
		SELECT n.nspname, c.relname, c.relkind::text, pg_get_userbyid(c.relowner),
		       CASE WHEN c.relispartition THEN (SELECT r.relnamespace::regnamespace::text || '.' || r.relname
		            FROM pg_class AS r WHERE r.oid = pg_partition_root(c.oid)) ELSE '' END
		FROM pg_class AS c JOIN pg_namespace AS n ON n.oid = c.relnamespace
		WHERE `+systemSchemas+` AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
		  AND NOT EXISTS (SELECT 1 FROM pg_depend AS d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
		ORDER BY 1, 2`)
	if err != nil {
		return nil, nil, fmt.Errorf("list cloud relations: %w", err)
	}
	objects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (object, error) {
		var o object
		return o, row.Scan(&o.schema, &o.name, &o.kind, &o.owner, &o.root)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list cloud relations: %w", err)
	}
	return schemas, objects, nil
}

func reconcileStatements(inventory Inventory, schemas []string, objects []object) ([]string, error) {
	tables, sequences := inventory.tableIndex(), inventory.sequenceIndex()
	roles := strings.Join(RuntimeRoles, ", ")
	var statements []string
	for _, schema := range schemas {
		statements = append(statements, fmt.Sprintf("REVOKE ALL ON SCHEMA %s FROM PUBLIC, %s", ident(schema), roles))
		if users := inventory.schemaUsers(schema); len(users) > 0 {
			statements = append(statements, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", ident(schema), strings.Join(users, ", ")))
		}
	}
	for _, o := range objects {
		grants, err := objectStatements(o, tables, sequences, roles)
		if err != nil {
			return nil, err
		}
		statements = append(statements, grants...)
	}
	if err := requireProvisioned(inventory, objects); err != nil {
		return nil, err
	}
	return statements, nil
}

func objectStatements(o object, tables map[string]Table, sequences map[string]Sequence, roles string) ([]string, error) {
	name := ident(o.schema) + "." + ident(o.name)
	switch {
	case o.root != "":
		// Partitions are reached through their root, whose policy governs them.
		if _, ok := tables[o.root]; !ok {
			return nil, fmt.Errorf("cloud partition %s has no write-path inventory entry for its root %s", o.qualified(), o.root)
		}
		return []string{fmt.Sprintf("REVOKE ALL ON %s FROM PUBLIC, %s", name, roles)}, nil
	case o.kind == "S":
		policy, ok := sequences[o.qualified()]
		if !ok || !policy.Provisioned {
			return nil, fmt.Errorf("cloud sequence %s has no provisioned write-path inventory entry", o.qualified())
		}
		statements := []string{fmt.Sprintf("REVOKE ALL ON SEQUENCE %s FROM PUBLIC, %s", name, roles)}
		if len(policy.Usage) > 0 {
			statements = append(statements, fmt.Sprintf("GRANT USAGE ON SEQUENCE %s TO %s", name, strings.Join(policy.Usage, ", ")))
		}
		return statements, nil
	case o.kind == "r" || o.kind == "p":
		policy, ok := tables[o.qualified()]
		if !ok {
			return nil, fmt.Errorf("cloud table %s has no write-path inventory entry%s", o.qualified(), orphanPartitionHint(o, tables))
		}
		if !policy.Provisioned {
			return nil, fmt.Errorf("cloud table %s exists but the inventory marks it unprovisioned", o.qualified())
		}
		return append([]string{fmt.Sprintf("REVOKE ALL ON %s FROM PUBLIC, %s", name, roles)}, tableGrants(policy)...), nil
	default:
		return nil, fmt.Errorf("cloud relation %s has kind %q, which the write-path inventory cannot describe", o.qualified(), o.kind)
	}
}

func requireProvisioned(inventory Inventory, objects []object) error {
	present := make(map[string]bool, len(objects))
	for _, o := range objects {
		present[o.qualified()] = true
	}
	for _, table := range inventory.Tables {
		if table.Provisioned && !present[table.Schema+"."+table.Name] {
			return fmt.Errorf("cloud write-path inventory marks %s.%s provisioned but it does not exist", table.Schema, table.Name)
		}
	}
	for _, sequence := range inventory.Sequences {
		if sequence.Provisioned && !present[sequence.Schema+"."+sequence.Name] {
			return fmt.Errorf("cloud write-path inventory marks sequence %s.%s provisioned but it does not exist", sequence.Schema, sequence.Name)
		}
	}
	return nil
}

func tableGrants(table Table) []string {
	name := ident(table.Schema) + "." + ident(table.Name)
	var statements []string
	for _, reader := range table.Readers {
		statements = append(statements, fmt.Sprintf("GRANT SELECT (%s) ON %s TO %s", strings.Join(reader.Columns, ", "), name, reader.Role))
	}
	for _, writer := range table.Writers {
		if len(writer.Insert) > 0 {
			statements = append(statements, fmt.Sprintf("GRANT INSERT (%s) ON %s TO %s", strings.Join(writer.Insert, ", "), name, writer.Role))
		}
		if len(writer.Update) > 0 {
			statements = append(statements, fmt.Sprintf("GRANT UPDATE (%s) ON %s TO %s", strings.Join(writer.Update, ", "), name, writer.Role))
		}
		if writer.Delete {
			statements = append(statements, fmt.Sprintf("GRANT DELETE ON %s TO %s", name, writer.Role))
		}
	}
	return statements
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// Privilege is one effective privilege of a runtime role. Column is "*" for a
// whole-relation privilege, the identity arguments for a function, and Object
// is empty for a schema privilege.
type Privilege struct {
	Role, Schema, Object, Column, Operation string
}

func (p Privilege) String() string {
	return fmt.Sprintf("%s %s on %s.%s(%s)", p.Role, p.Operation, p.Schema, p.Object, p.Column)
}

// ExpectedPrivileges is the exact effective privilege set the inventory grants.
func (inventory Inventory) ExpectedPrivileges() map[Privilege]bool {
	expected := make(map[Privilege]bool)
	for _, table := range inventory.Tables {
		if !table.Provisioned {
			continue
		}
		for _, reader := range table.Readers {
			for _, column := range reader.Columns {
				expected[Privilege{reader.Role, table.Schema, table.Name, column, "SELECT"}] = true
			}
		}
		for _, writer := range table.Writers {
			for _, column := range writer.Insert {
				expected[Privilege{writer.Role, table.Schema, table.Name, column, "INSERT"}] = true
			}
			for _, column := range writer.Update {
				expected[Privilege{writer.Role, table.Schema, table.Name, column, "UPDATE"}] = true
			}
			if writer.Delete {
				expected[Privilege{writer.Role, table.Schema, table.Name, "*", "DELETE"}] = true
			}
		}
	}
	for _, sequence := range inventory.Sequences {
		if !sequence.Provisioned {
			continue
		}
		for _, role := range sequence.Usage {
			expected[Privilege{role, sequence.Schema, sequence.Name, "*", "USAGE"}] = true
		}
	}
	for _, schema := range inventory.schemas() {
		for _, role := range inventory.schemaUsers(schema) {
			expected[Privilege{role, schema, "", "", "USAGE"}] = true
		}
	}
	for _, function := range inventory.Functions {
		for _, role := range function.Execute {
			expected[Privilege{role, function.Schema, function.Name, function.Arguments, "EXECUTE"}] = true
		}
	}
	return expected
}

// effectivePrivileges asks PostgreSQL what the runtime roles can actually do,
// including through ownership, membership, and PUBLIC.
const effectivePrivileges = `
WITH roles AS (SELECT unnest($1::text[]) AS role),
rels AS (
	SELECT c.oid, n.nspname, c.relname, c.relkind FROM pg_class AS c
	JOIN pg_namespace AS n ON n.oid = c.relnamespace
	WHERE ` + systemSchemas + ` AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f'))
SELECT r.role, rels.nspname, rels.relname, a.attname::text, p.op
FROM roles AS r CROSS JOIN rels
JOIN pg_attribute AS a ON a.attrelid = rels.oid AND a.attnum > 0 AND NOT a.attisdropped
CROSS JOIN unnest(ARRAY['SELECT', 'INSERT', 'UPDATE', 'REFERENCES']) AS p(op)
WHERE rels.relkind <> 'S' AND has_column_privilege(r.role, rels.oid, a.attnum, p.op)
UNION ALL
SELECT r.role, rels.nspname, rels.relname, '*', p.op
FROM roles AS r CROSS JOIN rels CROSS JOIN unnest(ARRAY['DELETE', 'TRUNCATE', 'TRIGGER']) AS p(op)
WHERE rels.relkind <> 'S' AND has_table_privilege(r.role, rels.oid, p.op)
UNION ALL
SELECT r.role, rels.nspname, rels.relname, '*', p.op
FROM roles AS r CROSS JOIN rels CROSS JOIN unnest(ARRAY['USAGE', 'SELECT', 'UPDATE']) AS p(op)
WHERE rels.relkind = 'S' AND has_sequence_privilege(r.role, rels.oid, p.op)
UNION ALL
SELECT r.role, n.nspname, '', '', p.op
FROM roles AS r CROSS JOIN pg_namespace AS n CROSS JOIN unnest(ARRAY['USAGE', 'CREATE']) AS p(op)
WHERE ` + systemSchemas + ` AND has_schema_privilege(r.role, n.oid, p.op)`

// EffectivePrivileges reports what the runtime roles can effectively do on
// every non-system schema and relation.
func EffectivePrivileges(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (map[Privilege]bool, error) {
	rows, err := q.Query(ctx, effectivePrivileges+"\nUNION ALL"+effectiveFunctionPrivileges, RuntimeRoles)
	if err != nil {
		return nil, fmt.Errorf("read effective cloud privileges: %w", err)
	}
	privileges, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Privilege, error) {
		var p Privilege
		return p, row.Scan(&p.Role, &p.Schema, &p.Object, &p.Column, &p.Operation)
	})
	if err != nil {
		return nil, fmt.Errorf("read effective cloud privileges: %w", err)
	}
	actual := make(map[Privilege]bool, len(privileges))
	for _, p := range privileges {
		actual[p] = true
	}
	return actual, nil
}

// VerifyEffectivePrivileges fails with ErrPrivilegeMismatch unless the
// runtime roles' effective privileges equal the inventory exactly.
func VerifyEffectivePrivileges(ctx context.Context, tx pgx.Tx, inventory Inventory) error {
	schemas, _, err := listObjects(ctx, tx)
	if err != nil {
		return err
	}
	inventory = inventory.Expand(schemas)
	actual, err := EffectivePrivileges(ctx, tx)
	if err != nil {
		return err
	}
	expected := inventory.ExpectedPrivileges()
	var differences []string
	for p := range actual {
		if !expected[p] {
			differences = append(differences, "extra "+p.String())
		}
	}
	for p := range expected {
		if !actual[p] {
			differences = append(differences, "missing "+p.String())
		}
	}
	if len(differences) > 0 {
		sort.Strings(differences)
		return fmt.Errorf("%w: %s", ErrPrivilegeMismatch, strings.Join(differences, "; "))
	}
	return nil
}

// orphanPartitionHint names the remedy when an unattached table looks like a
// leftover monthly partition (<root>_YYYYMM) of an inventoried table.
func orphanPartitionHint(o object, tables map[string]Table) string {
	for root := range tables {
		suffix := strings.TrimPrefix(o.qualified(), root+"_")
		if suffix != o.qualified() && len(suffix) == 6 && strings.Trim(suffix, "0123456789") == "" {
			return "; if it is an empty leftover partition from an interrupted partition pass, drop it (DROP TABLE " + o.qualified() + ")"
		}
	}
	return ""
}
