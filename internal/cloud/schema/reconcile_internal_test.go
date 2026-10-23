package schema

import (
	"slices"
	"strings"
	"testing"
)

func TestReconcileStatementsRevokeThenGrantExactly(t *testing.T) {
	inventory := Inventory{Tables: []Table{
		{Schema: "control", Name: "lease", Provisioned: true, Readers: []Reader{{Role: "goobers_controller", Columns: []string{"epoch"}}},
			Writers: []Writer{{Role: "goobers_controller", Update: []string{"epoch"}, Delete: true}}},
		{Schema: "control", Name: "future", Provisioned: false},
	}}
	statements, err := reconcileStatements(inventory, map[string][]string{"control": {"lease"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"REVOKE ALL ON SCHEMA control FROM PUBLIC, goobers_api, goobers_controller",
		"GRANT USAGE ON SCHEMA control TO goobers_api, goobers_controller",
		"REVOKE ALL ON control.lease FROM PUBLIC, goobers_api, goobers_controller",
		"GRANT SELECT (epoch) ON control.lease TO goobers_controller",
		"GRANT UPDATE (epoch) ON control.lease TO goobers_controller",
		"GRANT DELETE ON control.lease TO goobers_controller",
	}
	if !slices.Equal(statements, want) {
		t.Fatalf("statements:\n%s\nwant:\n%s", strings.Join(statements, "\n"), strings.Join(want, "\n"))
	}
}

func TestReconcileStatementsRefuseDisagreement(t *testing.T) {
	inventory := Inventory{Tables: []Table{
		{Schema: "control", Name: "lease", Provisioned: true},
		{Schema: "control", Name: "future", Provisioned: false},
	}}
	for name, tables := range map[string][]string{
		"table without policy":       {"lease", "stray"},
		"unprovisioned table exists": {"lease", "future"},
		"provisioned table missing":  {},
	} {
		if _, err := reconcileStatements(inventory, map[string][]string{"control": tables}); err == nil {
			t.Errorf("%s: reconcile accepted a table/inventory disagreement", name)
		}
	}
}
