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
	privilege := regexp.MustCompile(`(?i)\b(CREATE\s+ROLE|ALTER\s+ROLE|GRANT|REVOKE)\b`)
	for _, migration := range catalog {
		if match := privilege.FindString(migration.SQL); match != "" {
			t.Errorf("migration %s contains %q; privileges belong to the write-path inventory", migration.Name, match)
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
