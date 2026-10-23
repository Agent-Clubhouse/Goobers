package schema_test

import (
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/schema"
)

func TestColumnGrantsMatchWritePathInventory(t *testing.T) {
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var grantMigration string
	for _, migration := range catalog {
		if migration.Name == "0003_role_grants.expand.sql" {
			grantMigration = migration.SQL
			break
		}
	}
	if grantMigration == "" {
		t.Fatal("role grant migration is missing")
	}
	if want := schema.GrantMigrationSQL(inventory); grantMigration != want {
		t.Fatal("role grant migration differs from writepaths.yaml")
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
