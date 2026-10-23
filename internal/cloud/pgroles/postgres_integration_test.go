//go:build cloudintegration

package pgroles_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgroles"
)

// Roles are cluster-wide and shared with every other cloud test package, so
// these tests never drop them and never assume they are absent. They use a
// persistent non-superuser administrator (CREATEROLE, CREATEDB, as on Azure)
// that holds ADMIN on the runtime roles, as the provisioning administrator
// would, and drop only the databases they create.
const (
	instance    = "pgroles-test"
	admin       = "pgroles_admin"
	maintenance = "pgroles_maintenance"
)

type cluster struct {
	admin, superuser *pgx.Conn
	url              func(user, password, database string) string
	// maintenance is the administrator-owned database Provision runs on;
	// platform is the superuser-owned one, as on a managed server.
	maintenance, platform string
	// superuserIn connects as the superuser to a database.
	superuserIn func(t *testing.T, database string) *pgx.Conn
}

func setup(t *testing.T) cluster {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	superuser, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var closed bool
	if err := superuser.QueryRow(t.Context(), `SELECT NOT EXISTS (
		SELECT 1 FROM pg_database d, aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
		WHERE d.datname = current_database() AND a.grantee = 0 AND a.privilege_type = 'CONNECT')`).Scan(&closed); err != nil {
		t.Fatal(err)
	}
	if closed {
		t.Fatal("the test server's default database is closed to PUBLIC (CONNECT revoked): an earlier test ran pgroles.Provision on it. Provision closes the database it runs on, so tests must provision against a scratch maintenance database")
	}
	databases, err := pgroles.Databases(instance)
	if err != nil {
		t.Fatal(err)
	}
	dropDatabases := func() {
		for _, database := range append(databases, pgroles.Database{Name: "pgroles_foreign"}, pgroles.Database{Name: maintenance}) {
			_, _ = superuser.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{database.Name}.Sanitize()+" WITH (FORCE)")
		}
	}
	dropDatabases()
	t.Cleanup(func() { dropDatabases(); _ = superuser.Close(context.Background()) })
	exec(t, superuser, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgroles_admin') THEN
			CREATE ROLE pgroles_admin LOGIN CREATEROLE CREATEDB PASSWORD 'admin';
		END IF; END $$`)
	for _, role := range pgroles.Roles() {
		exec(t, superuser, `DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+role+`') THEN
				GRANT `+role+` TO pgroles_admin WITH ADMIN TRUE, INHERIT FALSE, SET TRUE;
			END IF; END $$`)
	}
	// Provision runs on a maintenance database the administrator owns, so it
	// can close it to the contract roles (as a self-managed server allows).
	exec(t, superuser, "CREATE DATABASE "+maintenance+" OWNER pgroles_admin")
	base, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	urlFor := func(user, password, database string) string {
		u := *base
		u.User = url.UserPassword(user, password)
		u.Path = "/" + database
		return u.String()
	}
	adminConn, err := pgx.Connect(t.Context(), urlFor(admin, "admin", maintenance))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminConn.Close(context.Background()) })
	superuserIn := func(t *testing.T, database string) *pgx.Conn {
		u := *base
		u.Path = "/" + database
		conn, err := pgx.Connect(t.Context(), u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}
	return cluster{admin: adminConn, superuser: superuser, url: urlFor, maintenance: maintenance,
		platform: strings.TrimPrefix(base.Path, "/"), superuserIn: superuserIn}
}

func exec(t *testing.T, conn *pgx.Conn, statement string) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func provision(t *testing.T, c cluster) bool {
	t.Helper()
	changed, err := pgroles.Provision(t.Context(), c.admin, pgroles.Plan{Instance: instance})
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func setPasswords(t *testing.T, c cluster) {
	t.Helper()
	for _, role := range pgroles.Roles() {
		exec(t, c.admin, "ALTER ROLE "+role+" PASSWORD 'pw'")
	}
}

func connects(t *testing.T, connString string) bool {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), connString)
	if err == nil {
		_ = conn.Close(t.Context())
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return false
	}
	t.Fatalf("connect: %v", err)
	return false
}

func assertConnectMatrix(t *testing.T, c cluster) {
	t.Helper()
	goobers, _ := pgroles.InstanceDatabase(instance)
	for _, check := range []struct {
		role, database string
		want           bool
	}{
		{pgroles.Owner, goobers, true},
		{pgroles.API, goobers, true},
		{pgroles.Controller, goobers, true},
		{pgroles.Temporal, goobers, false},
		{pgroles.Temporal, "temporal", true},
		{pgroles.Temporal, "temporal_visibility", true},
		{pgroles.API, "temporal", false},
		{pgroles.Owner, "temporal_visibility", false},
		{pgroles.API, c.maintenance, false},
		{pgroles.Owner, c.maintenance, false},
		{pgroles.Temporal, c.maintenance, false},
	} {
		if got := connects(t, c.url(check.role, "pw", check.database)); got != check.want {
			t.Fatalf("%s connecting to %s = %v, want %v", check.role, check.database, got, check.want)
		}
	}
}

func TestProvisionIsolatesRolesAndIsIdempotent(t *testing.T) {
	c := setup(t)
	if !provision(t, c) {
		t.Fatal("first Provision created no databases")
	}
	if provision(t, c) {
		t.Fatal("second Provision changed something; want an idempotent no-op")
	}
	setPasswords(t, c)
	assertConnectMatrix(t, c)
	if !connects(t, c.url(admin, "admin", c.maintenance)) {
		t.Fatal("closing the maintenance database to the contract roles locked out the administrator")
	}
	var privileged int
	if err := c.admin.QueryRow(t.Context(), `
		SELECT count(*) FROM pg_roles
		WHERE rolname = ANY($1) AND (rolsuper OR rolcreaterole OR rolcreatedb OR rolbypassrls OR NOT rolcanlogin)`,
		pgroles.Roles()).Scan(&privileged); err != nil {
		t.Fatal(err)
	}
	if privileged != 0 {
		t.Fatalf("%d provisioned roles are privileged or cannot log in", privileged)
	}
}

// TestProvisionRevokesForeignConnect: explicit grants someone else made, on
// the contract's databases or any other, do not survive Provision.
func TestProvisionRevokesForeignConnect(t *testing.T) {
	c := setup(t)
	provision(t, c)
	exec(t, c.superuser, "CREATE DATABASE pgroles_foreign OWNER pgroles_admin")
	exec(t, c.superuser, "GRANT CONNECT, TEMPORARY ON DATABASE temporal TO goobers_api")
	exec(t, c.superuser, "GRANT CONNECT ON DATABASE pgroles_foreign TO goobers_controller")
	exec(t, c.superuser, "REVOKE CONNECT ON DATABASE pgroles_foreign FROM PUBLIC")
	provision(t, c)
	setPasswords(t, c)
	assertConnectMatrix(t, c)
	if connects(t, c.url(pgroles.Controller, "pw", "pgroles_foreign")) {
		t.Fatal("goobers_controller keeps a foreign database grant after Provision")
	}

	// A grant on a database the administrator cannot act for is refused by
	// name, not silently left in place.
	exec(t, c.superuser, "ALTER DATABASE pgroles_foreign OWNER TO "+superuserName(t, c))
	exec(t, c.superuser, "GRANT CONNECT ON DATABASE pgroles_foreign TO goobers_controller")
	if _, err := pgroles.Provision(t.Context(), c.admin, pgroles.Plan{Instance: instance}); !errors.Is(err, pgroles.ErrRoleDrift) {
		t.Fatalf("Provision over an unmanageable foreign grant = %v, want ErrRoleDrift", err)
	}
}

func superuserName(t *testing.T, c cluster) string {
	t.Helper()
	var name string
	if err := c.superuser.QueryRow(t.Context(), "SELECT current_user").Scan(&name); err != nil {
		t.Fatal(err)
	}
	return pgx.Identifier{name}.Sanitize()
}

// TestProvisionConvergesDriftedRoleAttributes: a superuser administrator
// resets drifted attributes; a non-superuser one that cannot is refused by
// name; a role membership is always refused.
func TestProvisionConvergesOrRefusesDrift(t *testing.T) {
	c := setup(t)
	provision(t, c)
	exec(t, c.superuser, "ALTER ROLE goobers_controller NOLOGIN CREATEDB")
	// The superuser runs on the test's maintenance database too, never on the
	// shared default database, which Provision would close.
	superuser := c.superuserIn(t, c.maintenance)
	if _, err := pgroles.Provision(t.Context(), superuser, pgroles.Plan{Instance: instance}); err != nil {
		t.Fatalf("superuser Provision did not converge drifted attributes: %v", err)
	}
	var login, createdb bool
	if err := c.admin.QueryRow(t.Context(), "SELECT rolcanlogin, rolcreatedb FROM pg_roles WHERE rolname='goobers_controller'").Scan(&login, &createdb); err != nil {
		t.Fatal(err)
	}
	if !login || createdb {
		t.Fatalf("goobers_controller login=%v createdb=%v after convergence", login, createdb)
	}

	exec(t, c.superuser, "ALTER ROLE goobers_controller BYPASSRLS")
	t.Cleanup(func() { _, _ = c.superuser.Exec(context.Background(), "ALTER ROLE goobers_controller NOBYPASSRLS") })
	if _, err := pgroles.Provision(t.Context(), c.admin, pgroles.Plan{Instance: instance}); !errors.Is(err, pgroles.ErrRoleDrift) {
		t.Fatalf("non-superuser Provision over a BYPASSRLS role = %v, want ErrRoleDrift", err)
	}
	exec(t, c.superuser, "ALTER ROLE goobers_controller NOBYPASSRLS")

	exec(t, c.superuser, "GRANT pg_read_all_data TO goobers_api")
	t.Cleanup(func() { _, _ = c.superuser.Exec(context.Background(), "REVOKE pg_read_all_data FROM goobers_api") })
	for name, conn := range map[string]*pgx.Conn{"superuser": superuser, "administrator": c.admin} {
		if _, err := pgroles.Provision(t.Context(), conn, pgroles.Plan{Instance: instance}); !errors.Is(err, pgroles.ErrRoleDrift) {
			t.Fatalf("%s Provision over a role membership = %v, want ErrRoleDrift", name, err)
		}
	}
}

// TestOwnerAppliesMigrationsAndAppRolesCannot runs the schema migrations as
// goobers_owner on the provisioned database, and shows the api role cannot
// change the schema.
func TestOwnerAppliesMigrationsAndAppRolesCannot(t *testing.T) {
	c := setup(t)
	provision(t, c)
	setPasswords(t, c)
	goobers, _ := pgroles.InstanceDatabase(instance)
	owner := openPool(t, c.url(pgroles.Owner, "pw", goobers))
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := migrate.Runner{Direct: owner, Catalog: catalog, BackupRef: "pgroles-test", InstanceID: "instance", Holder: "migrate"}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatalf("goobers_owner could not apply migrations: %v", err)
	}
	api, err := pgx.Connect(t.Context(), c.url(pgroles.API, "pw", goobers))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = api.Close(context.Background()) }()
	for _, statement := range []string{
		"CREATE TABLE control.api_owned (id integer)",
		"DROP TABLE control.lease",
		"CREATE SCHEMA api_schema",
	} {
		if _, err := api.Exec(t.Context(), statement); err == nil {
			t.Fatalf("goobers_api ran DDL: %s", statement)
		}
	}
}

// TestCreatesEntraPrincipals stands in for Azure's
// pgaadauth_create_principal_with_oid on a test-owned role.
func TestCreatesEntraPrincipals(t *testing.T) {
	c := setup(t)
	const probe = "pgroles_entra_probe"
	exec(t, c.superuser, "DROP ROLE IF EXISTS "+probe)
	stub := c.superuserIn(t, c.maintenance)
	exec(t, stub, `
		CREATE TABLE IF NOT EXISTS public.pgroles_entra_calls (role text, object_id text, kind text);
		TRUNCATE public.pgroles_entra_calls;
		GRANT SELECT, INSERT ON public.pgroles_entra_calls TO pgroles_admin;
		CREATE OR REPLACE FUNCTION public.pgaadauth_create_principal_with_oid(role text, object_id text, kind text, is_admin boolean, is_mfa boolean)
		RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			EXECUTE format('CREATE ROLE %I LOGIN', role);
			INSERT INTO public.pgroles_entra_calls VALUES (role, object_id, kind);
			RETURN 'Created role for ' || role;
		END $$`)
	// The stub lives in the maintenance database, which setup drops.
	t.Cleanup(func() { _, _ = c.superuser.Exec(context.Background(), "DROP ROLE IF EXISTS "+probe) })
	if _, err := pgroles.EnsureRoleForTest(t.Context(), c.admin, probe, "oid-probe"); err != nil {
		t.Fatal(err)
	}
	var role, objectID, kind string
	if err := c.admin.QueryRow(t.Context(), "SELECT role, object_id, kind FROM public.pgroles_entra_calls").Scan(&role, &objectID, &kind); err != nil {
		t.Fatalf("no Entra principal was created: %v", err)
	}
	if role != probe || objectID != "oid-probe" || kind != "service" {
		t.Fatalf("Entra principal = (%s, %s, %s)", role, objectID, kind)
	}
	if _, err := pgroles.Provision(t.Context(), c.admin, pgroles.Plan{Instance: instance, Entra: map[string]string{pgroles.Temporal: "oid"}}); err == nil {
		t.Fatal("temporal_app was accepted as an Entra principal; Temporal needs a password role")
	}
}

func openPool(t *testing.T, connString string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestProvisionOnPlatformOwnedMaintenanceDatabase: where the platform owns
// the maintenance database (managed servers), Provision cannot close it and
// still succeeds; the contract databases stay isolated.
func TestProvisionOnPlatformOwnedMaintenanceDatabase(t *testing.T) {
	c := setup(t)
	platform, err := pgx.Connect(t.Context(), c.url(admin, "admin", c.platform))
	if err != nil {
		t.Skipf("administrator cannot reach the platform database: %v", err)
	}
	defer func() { _ = platform.Close(context.Background()) }()
	if _, err := pgroles.Provision(t.Context(), platform, pgroles.Plan{Instance: instance}); err != nil {
		t.Fatalf("Provision on a platform-owned maintenance database: %v", err)
	}
	setPasswords(t, c)
	if !connects(t, c.url(pgroles.API, "pw", c.platform)) {
		t.Fatal("Provision closed a platform-owned maintenance database it cannot act for")
	}
}
