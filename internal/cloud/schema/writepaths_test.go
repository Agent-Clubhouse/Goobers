package schema_test

import (
	"regexp"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/schema"
)

// TestMigrationsNeverManageRolesOrGrants keeps privileges out of versioned
// migrations: role provisioning creates roles and ReconcileGrants converges
// grants on every apply, so an inventory change never rewrites an applied
// migration.
func TestMigrationsNeverManageRolesOrGrants(t *testing.T) {
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range catalog {
		if match := privilegeDDL.FindString(migration.SQL); match != "" {
			t.Errorf("migration %s contains %q; privileges belong to the write-path inventory", migration.Name, match)
		}
	}
}

// privilegeDDL matches statements that create or change roles, privileges,
// ownership or execution identity.
var privilegeDDL = regexp.MustCompile(`(?i)\b((CREATE|ALTER|DROP)\s+(ROLE|USER|GROUP)|GRANT|REVOKE|OWNER\s+TO|SECURITY\s+DEFINER|SET\s+(LOCAL\s+|SESSION\s+)?ROLE|SET\s+SESSION\s+AUTHORIZATION)\b`)

func TestMigrationGuardCatchesEquivalentDDL(t *testing.T) {
	for _, statement := range []string{
		"CREATE ROLE sneaky NOLOGIN", "create user sneaky", "CREATE GROUP sneaky", "ALTER ROLE goobers_api SUPERUSER",
		"ALTER USER goobers_api CREATEROLE", "DROP ROLE goobers_api", "DROP USER x", "GRANT SELECT ON control.lease TO PUBLIC",
		"REVOKE ALL ON control.lease FROM goobers_api", "ALTER TABLE control.lease OWNER TO goobers_controller",
		"CREATE FUNCTION control.f() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'select 1'",
		"SET ROLE goobers_owner", "SET LOCAL ROLE goobers_owner", "SET SESSION AUTHORIZATION goobers_owner",
	} {
		if !privilegeDDL.MatchString(statement) {
			t.Errorf("migration guard misses %q", statement)
		}
	}
	for _, statement := range []string{"CREATE TABLE control.grants_log (id int)", "CREATE INDEX lease_owner_idx ON control.lease (holder)"} {
		if privilegeDDL.MatchString(statement) {
			t.Errorf("migration guard rejects ordinary DDL %q", statement)
		}
	}
}

func TestApiRoleCannotWriteAcceptanceDispatching(t *testing.T) {
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	writer, ok := inventory.WriterFor("control", "acceptance", "goobers_api")
	if !ok {
		t.Fatal("acceptance API writer policy is missing")
	}
	if slices.Contains(writer.Insert, "state") || slices.Contains(writer.Update, "state") ||
		slices.Contains(writer.Insert, "owner_epoch") || slices.Contains(writer.Update, "owner_epoch") {
		t.Fatalf("API acceptance policy permits dispatch authority: %+v", writer)
	}
}

func TestInventoryRejectsUnknownRoles(t *testing.T) {
	inventory := schema.Inventory{Tables: []schema.Table{{
		Schema: "control", Name: "lease", Provisioned: true, Fence: schema.FenceEpoch, Rationale: "test",
		Writers: []schema.Writer{{Role: "temporal_app", Update: []string{"epoch"}}},
	}}}
	if err := inventory.Validate(); err == nil {
		t.Fatal("inventory granted a role outside the runtime roles")
	}
}
