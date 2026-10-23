// Package pgroles is the PostgreSQL role and database contract shared by the
// Azure (infra/bicep) and CloudNativePG (deploy/reference/goobers-cloud/cnpg)
// references, and the idempotent provisioning that goobers-cloud bootstrap
// runs as the server administrator. Table and column grants are not here:
// the schema migrations derive them from the write-path inventory.
package pgroles

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Login roles. Each workload connects as exactly one of them; none is a
// superuser and none is granted another.
const (
	// Owner owns the instance database and schema; only the migrate Job uses it.
	Owner = "goobers_owner"
	// API is the api role.
	API = "goobers_api"
	// Controller is the controller role.
	Controller = "goobers_controller"
	// Temporal owns the Temporal databases and has no grant on goobers ones.
	Temporal = "temporal_app"
)

// Roles lists every login role the references declare.
func Roles() []string { return []string{Owner, API, Controller, Temporal} }

// Database is one database, its owner, and the other roles that may connect.
type Database struct {
	Name    string
	Owner   string
	Connect []string
}

var instanceName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// InstanceDatabase names an instance's database: goobers_<instance>, lower
// case, with dashes as underscores (as infra/bicep/modules/postgres.bicep).
func InstanceDatabase(instance string) (string, error) {
	name := strings.ReplaceAll(strings.ToLower(instance), "-", "_")
	if !instanceName.MatchString(name) {
		return "", fmt.Errorf("pgroles: instance %q does not form a database name", instance)
	}
	return "goobers_" + name, nil
}

// Databases returns the databases one server hosts for instance.
func Databases(instance string) ([]Database, error) {
	goobers, err := InstanceDatabase(instance)
	if err != nil {
		return nil, err
	}
	return []Database{
		{Name: goobers, Owner: Owner, Connect: []string{API, Controller}},
		{Name: "temporal", Owner: Temporal},
		{Name: "temporal_visibility", Owner: Temporal},
	}, nil
}

// Plan selects how missing roles are created.
type Plan struct {
	Instance string
	// Entra maps a goobers role to the object ID of its workload identity. A
	// missing mapped role is created as an Entra principal (Azure); any other
	// missing role is a plain LOGIN role whose password the operator manages
	// (CNPG managed roles, or ALTER ROLE for temporal_app on Azure).
	Entra map[string]string
}

// ErrRoleDrift means a pre-existing role differs from the contract in a way
// Provision may not or cannot correct: a role membership, an attribute the
// administrator lacks the right to reset, or a role the administrator cannot
// manage. Provision refuses rather than continue with an unsafe role.
var ErrRoleDrift = errors.New("pgroles: existing role differs from the role contract")

// Provision creates missing roles and databases, converges existing roles'
// attributes and every database's CONNECT grants for these roles, and then
// verifies the result. It runs as the server administrator (no superuser
// needed) on a maintenance database and is idempotent: changed is false when
// everything already matched.
func Provision(ctx context.Context, conn *pgx.Conn, plan Plan) (changed bool, err error) {
	databases, err := Databases(plan.Instance)
	if err != nil {
		return false, err
	}
	changed, err = ProvisionRoles(ctx, conn, plan.Entra)
	if err != nil {
		return changed, err
	}
	for _, database := range databases {
		updated, err := ensureDatabase(ctx, conn, database)
		if err != nil {
			return changed, err
		}
		changed = changed || updated
	}
	if err := revokeForeignGrants(ctx, conn, databases); err != nil {
		return changed, err
	}
	return changed, verifyConnect(ctx, conn, databases)
}

// ProvisionRoles creates missing roles and converges existing ones to the
// contract (LOGIN; no superuser, createrole, createdb, bypassrls or
// replication; no role membership). Test fixtures use it to create the
// cluster-wide runtime roles the same way bootstrap does.
func ProvisionRoles(ctx context.Context, conn *pgx.Conn, entra map[string]string) (changed bool, err error) {
	for role, objectID := range entra {
		if role == Temporal || !isRole(role) || objectID == "" {
			return false, fmt.Errorf("pgroles: no Entra principal for role %q", role)
		}
	}
	for _, role := range Roles() {
		updated, err := ensureRole(ctx, conn, role, entra[role])
		if err != nil {
			return changed, err
		}
		changed = changed || updated
	}
	return changed, nil
}

func isRole(name string) bool { return slices.Contains(Roles(), name) }

func ensureRole(ctx context.Context, conn *pgx.Conn, role, entraObjectID string) (bool, error) {
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&exists); err != nil {
		return false, fmt.Errorf("pgroles: look up role %s: %w", role, err)
	}
	if !exists {
		var err error
		if entraObjectID != "" {
			_, err = conn.Exec(ctx, "SELECT pgaadauth_create_principal_with_oid($1, $2, 'service', false, false)", role, entraObjectID)
		} else {
			_, err = conn.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN")
		}
		if err != nil {
			return false, fmt.Errorf("pgroles: create role %s: %w", role, err)
		}
	}
	// A non-superuser administrator (Azure's azure_pg_admin) must be able to
	// SET ROLE to an owner to assign it a database, without inheriting its rights.
	if err := grantSetToAdministrator(ctx, conn, role); err != nil {
		return false, err
	}
	converged, err := convergeRole(ctx, conn, role)
	return !exists || converged, err
}

// convergeRole resets a drifted role's attributes, and refuses role
// memberships, which could carry any privilege.
func convergeRole(ctx context.Context, conn *pgx.Conn, role string) (bool, error) {
	var login, privileged, member bool
	if err := conn.QueryRow(ctx, `
		SELECT r.rolcanlogin, r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls OR r.rolreplication,
		       EXISTS (SELECT 1 FROM pg_auth_members AS m WHERE m.member = r.oid)
		FROM pg_roles AS r WHERE r.rolname = $1`, role).Scan(&login, &privileged, &member); err != nil {
		return false, fmt.Errorf("pgroles: inspect role %s: %w", role, err)
	}
	if member {
		return false, fmt.Errorf("%w: %s is a member of another role; remove the membership", ErrRoleDrift, role)
	}
	if login && !privileged {
		return false, nil
	}
	if _, err := conn.Exec(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+
		" LOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB NOBYPASSRLS NOREPLICATION"); err != nil {
		return false, fmt.Errorf("%w: %s has login=%v and privileged attributes=%v, and the administrator cannot reset them: %w",
			ErrRoleDrift, role, login, privileged, err)
	}
	return true, nil
}

func grantSetToAdministrator(ctx context.Context, conn *pgx.Conn, role string) error {
	var superuser, member bool
	if err := conn.QueryRow(ctx, `
		SELECT rolsuper, pg_has_role(current_user, $1, 'SET') FROM pg_roles WHERE rolname=current_user`,
		role).Scan(&superuser, &member); err != nil {
		return fmt.Errorf("pgroles: inspect administrator: %w", err)
	}
	if superuser || member {
		return nil
	}
	if _, err := conn.Exec(ctx, "GRANT "+pgx.Identifier{role}.Sanitize()+" TO CURRENT_USER WITH INHERIT FALSE, SET TRUE"); err != nil {
		return fmt.Errorf("%w: the administrator cannot manage existing role %s (created by another role without ADMIN for it): %w", ErrRoleDrift, role, err)
	}
	return nil
}

func ensureDatabase(ctx context.Context, conn *pgx.Conn, database Database) (bool, error) {
	name, owner := pgx.Identifier{database.Name}.Sanitize(), pgx.Identifier{database.Owner}.Sanitize()
	var current string
	err := conn.QueryRow(ctx, `
		SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1`, database.Name).Scan(&current)
	changed := false
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// CREATE DATABASE cannot run in a transaction; pgx sends it alone.
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+name+" OWNER "+owner); err != nil {
			return false, fmt.Errorf("pgroles: create database %s: %w", database.Name, err)
		}
		changed = true
	case err != nil:
		return false, fmt.Errorf("pgroles: look up database %s: %w", database.Name, err)
	case current != database.Owner:
		if _, err := conn.Exec(ctx, "ALTER DATABASE "+name+" OWNER TO "+owner); err != nil {
			return false, fmt.Errorf("pgroles: set owner of %s: %w", database.Name, err)
		}
		changed = true
	}
	statements := []string{"REVOKE ALL ON DATABASE " + name + " FROM PUBLIC"}
	for _, role := range Roles() {
		if role != database.Owner {
			statements = append(statements, "REVOKE ALL ON DATABASE "+name+" FROM "+pgx.Identifier{role}.Sanitize())
		}
	}
	for _, role := range database.Connect {
		statements = append(statements, "GRANT CONNECT ON DATABASE "+name+" TO "+pgx.Identifier{role}.Sanitize())
	}
	return changed, asOwner(ctx, conn, database.Owner, statements)
}

// asOwner runs statements as a database's owner. Only a grant's grantor can
// revoke it, and grants a superuser issues are recorded as the owner's, so a
// non-superuser administrator's REVOKE would otherwise warn and change nothing.
func asOwner(ctx context.Context, conn *pgx.Conn, owner string, statements []string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgroles: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range append([]string{"SET LOCAL ROLE " + pgx.Identifier{owner}.Sanitize()}, statements...) {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("pgroles: %s: %w", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgroles: commit: %w", err)
	}
	return nil
}

// revokeForeignGrants removes explicit privileges for these roles on every
// other database in the cluster, so no role reaches a database outside the
// contract through a grant someone else made.
func revokeForeignGrants(ctx context.Context, conn *pgx.Conn, databases []Database) error {
	targets := make([]string, len(databases))
	for i, database := range databases {
		targets[i] = database.Name
	}
	rows, err := conn.Query(ctx, `
		SELECT DISTINCT d.datname, pg_get_userbyid(d.datdba), r.rolname
		FROM pg_database AS d CROSS JOIN LATERAL aclexplode(d.datacl) AS a
		JOIN pg_roles AS r ON r.oid = a.grantee
		WHERE NOT d.datistemplate AND d.datname <> ALL($1) AND r.rolname = ANY($2)
		ORDER BY 1, 3`, targets, Roles())
	if err != nil {
		return fmt.Errorf("pgroles: list foreign database grants: %w", err)
	}
	type grant struct{ database, owner, role string }
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (grant, error) {
		var g grant
		return g, row.Scan(&g.database, &g.owner, &g.role)
	})
	if err != nil {
		return fmt.Errorf("pgroles: list foreign database grants: %w", err)
	}
	for _, g := range grants {
		if g.owner == g.role {
			return fmt.Errorf("%w: %s owns database %s outside the contract", ErrRoleDrift, g.role, g.database)
		}
		statement := "REVOKE ALL ON DATABASE " + pgx.Identifier{g.database}.Sanitize() + " FROM " + pgx.Identifier{g.role}.Sanitize()
		if err := asOwner(ctx, conn, g.owner, []string{statement}); err != nil {
			return fmt.Errorf("%w: cannot remove %s's grant on database %s: %w", ErrRoleDrift, g.role, g.database, err)
		}
	}
	return nil
}

// verifyConnect checks each role's effective database privileges on the
// target databases: CONNECT exactly where the contract allows, and no
// TEMPORARY or CREATE except as the owner.
func verifyConnect(ctx context.Context, conn *pgx.Conn, databases []Database) error {
	for _, database := range databases {
		for _, role := range Roles() {
			var connect, other bool
			if err := conn.QueryRow(ctx, `
				SELECT has_database_privilege($1, $2, 'CONNECT'),
				       has_database_privilege($1, $2, 'TEMPORARY') OR has_database_privilege($1, $2, 'CREATE')`,
				role, database.Name).Scan(&connect, &other); err != nil {
				return fmt.Errorf("pgroles: verify %s on %s: %w", role, database.Name, err)
			}
			owner := role == database.Owner
			if connect != (owner || slices.Contains(database.Connect, role)) || (other && !owner) {
				return fmt.Errorf("%w: %s has CONNECT=%v, TEMPORARY/CREATE=%v on %s after provisioning", ErrRoleDrift, role, connect, other, database.Name)
			}
		}
	}
	return nil
}
