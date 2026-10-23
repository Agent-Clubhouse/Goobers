package schema

import (
	"slices"
	"strings"
	"testing"
)

func testInventory() Inventory {
	return Inventory{
		Tables: []Table{
			{Schema: "control", Name: "lease", Provisioned: true, Readers: []Reader{{Role: "goobers_controller", Columns: []string{"epoch"}}},
				Writers: []Writer{{Role: "goobers_controller", Update: []string{"epoch"}, Delete: true}}},
			{Schema: "journal", Name: "event", Provisioned: true, Readers: []Reader{{Role: "goobers_api", Columns: []string{"seq"}}}},
			{Schema: "control", Name: "future", Provisioned: false},
		},
		Sequences: []Sequence{{Schema: "control", Name: "generation", Provisioned: true, Rationale: "test", Usage: []string{"goobers_controller"}}},
	}
}

func TestReconcileStatementsCoverEverySchemaAndKind(t *testing.T) {
	objects := []object{
		{schema: "control", name: "generation", kind: "S"},
		{schema: "control", name: "lease", kind: "r"},
		{schema: "journal", name: "event", kind: "p"},
		{schema: "journal", name: "event_2026_10", kind: "r", root: "journal.event"},
	}
	statements, err := reconcileStatements(testInventory(), []string{"control", "journal", "public"}, objects)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`REVOKE ALL ON SCHEMA "control" FROM PUBLIC, goobers_api, goobers_controller`,
		`GRANT USAGE ON SCHEMA "control" TO goobers_controller`,
		`REVOKE ALL ON SCHEMA "journal" FROM PUBLIC, goobers_api, goobers_controller`,
		`GRANT USAGE ON SCHEMA "journal" TO goobers_api`,
		`REVOKE ALL ON SCHEMA "public" FROM PUBLIC, goobers_api, goobers_controller`,
		`REVOKE ALL ON SEQUENCE "control"."generation" FROM PUBLIC, goobers_api, goobers_controller`,
		`GRANT USAGE ON SEQUENCE "control"."generation" TO goobers_controller`,
		`REVOKE ALL ON "control"."lease" FROM PUBLIC, goobers_api, goobers_controller`,
		`GRANT SELECT (epoch) ON "control"."lease" TO goobers_controller`,
		`GRANT UPDATE (epoch) ON "control"."lease" TO goobers_controller`,
		`GRANT DELETE ON "control"."lease" TO goobers_controller`,
		`REVOKE ALL ON "journal"."event" FROM PUBLIC, goobers_api, goobers_controller`,
		`GRANT SELECT (seq) ON "journal"."event" TO goobers_api`,
		`REVOKE ALL ON "journal"."event_2026_10" FROM PUBLIC, goobers_api, goobers_controller`,
	}
	if !slices.Equal(statements, want) {
		t.Fatalf("statements:\n%s\nwant:\n%s", strings.Join(statements, "\n"), strings.Join(want, "\n"))
	}
}

func TestReconcileStatementsRefuseDisagreement(t *testing.T) {
	base := []object{
		{schema: "control", name: "generation", kind: "S"},
		{schema: "control", name: "lease", kind: "r"},
		{schema: "journal", name: "event", kind: "p"},
	}
	for name, extra := range map[string][]object{
		"table without policy in an uninventoried schema": {{schema: "readmodel_v2", name: "runs", kind: "r"}},
		"table without policy in public":                  {{schema: "public", name: "stray", kind: "r"}},
		"unprovisioned table exists":                      {{schema: "control", name: "future", kind: "r"}},
		"sequence without policy":                         {{schema: "control", name: "other_seq", kind: "S"}},
		"partition of an uninventoried root":              {{schema: "journal", name: "x_1", kind: "r", root: "journal.x"}},
		"view":                                            {{schema: "control", name: "lease_view", kind: "v"}},
	} {
		if _, err := reconcileStatements(testInventory(), []string{"control", "journal", "public", "readmodel_v2"}, append(slices.Clone(base), extra...)); err == nil {
			t.Errorf("%s: reconcile accepted it", name)
		}
	}
	if _, err := reconcileStatements(testInventory(), []string{"control"}, base[:2]); err == nil {
		t.Error("provisioned table missing: reconcile accepted it")
	}
}

func TestExpectedPrivilegesIncludeSequencesAndSchemas(t *testing.T) {
	expected := testInventory().ExpectedPrivileges()
	for _, p := range []Privilege{
		{"goobers_controller", "control", "generation", "*", "USAGE"},
		{"goobers_controller", "control", "", "", "USAGE"},
		{"goobers_api", "journal", "", "", "USAGE"},
		{"goobers_controller", "control", "lease", "*", "DELETE"},
	} {
		if !expected[p] {
			t.Errorf("expected privileges lack %s", p)
		}
	}
	if expected[Privilege{"goobers_api", "control", "", "", "USAGE"}] {
		t.Error("goobers_api gets USAGE on a schema where the inventory grants it nothing")
	}
}

func TestFunctionStatementsRevokeAndRefuseDefiners(t *testing.T) {
	inventory := Inventory{Functions: []Function{
		{Schema: "control", Name: "allowed", Arguments: "integer", Rationale: "test", Execute: []string{"goobers_controller"}},
		{Schema: "control", Name: "definer", Arguments: "", Rationale: "test", SecurityDefiner: true},
	}}
	statements, err := functionStatements(inventory, []function{
		{schema: "control", name: "allowed", arguments: "integer", owner: "goobers_owner"},
		{schema: "control", name: "guard", arguments: "", owner: "goobers_owner"},
		{schema: "control", name: "definer", arguments: "", owner: "goobers_owner", securityDefiner: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`REVOKE ALL ON FUNCTION "control"."allowed"(integer) FROM PUBLIC, goobers_api, goobers_controller`,
		`GRANT EXECUTE ON FUNCTION "control"."allowed"(integer) TO goobers_controller`,
		`REVOKE ALL ON FUNCTION "control"."guard"() FROM PUBLIC, goobers_api, goobers_controller`,
		`REVOKE ALL ON FUNCTION "control"."definer"() FROM PUBLIC, goobers_api, goobers_controller`,
	}
	if !slices.Equal(statements, want) {
		t.Fatalf("statements:\n%s\nwant:\n%s", strings.Join(statements, "\n"), strings.Join(want, "\n"))
	}
	for name, f := range map[string]function{
		"undeclared definer":          {schema: "control", name: "sneaky", owner: "goobers_owner", securityDefiner: true},
		"definer declared as invoker": {schema: "control", name: "allowed", arguments: "integer", owner: "goobers_owner", securityDefiner: true},
		"runtime-role owner":          {schema: "control", name: "guard", owner: "goobers_controller"},
	} {
		if _, err := functionStatements(inventory, []function{f}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
