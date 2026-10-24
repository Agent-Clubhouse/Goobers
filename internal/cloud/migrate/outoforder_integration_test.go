//go:build cloudintegration

package migrate

import (
	"slices"
	"strings"
	"testing"
)

// TestApplyWarnsButAppliesAMigrationBelowTheAppliedMaximum: a migration
// numbered below one the database already has is applied, after the ones
// written to follow it; the runner reports that rather than refusing (the
// pre-merge gate, test/migrationorder, is what refuses the number).
func TestApplyWarnsButAppliesAMigrationBelowTheAppliedMaximum(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	report := func(err error) { warnings = append(warnings, err.Error()) }
	if err := (Runner{Direct: pool, Catalog: catalog, BackupRef: "test", InstanceID: "migration", Holder: "test", Report: report}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("an in-order apply warned: %v", warnings)
	}
	last := catalog[len(catalog)-1]
	gap := 0
	for version := last.Version - 1; version > leaseSchemaVersion; version-- {
		if !slices.ContainsFunc(catalog, func(m Migration) bool { return m.Version == version }) {
			gap = version
			break
		}
	}
	if gap == 0 {
		t.Skip("the catalog has no gap below its highest migration to probe")
	}
	probe := func(version int, column string) Migration {
		m := last
		m.Version, m.Name = version, "probe"
		m.SQL = "ALTER TABLE control.claim ADD COLUMN " + column + " text"
		return m
	}
	late := append(slices.Clone(catalog), probe(gap, "probe_late"))
	slices.SortFunc(late, func(a, b Migration) int { return a.Version - b.Version })
	if err := (Runner{Direct: pool, Catalog: late, BackupRef: "test", InstanceID: "migration", Holder: "test", Report: report}).Apply(t.Context()); err != nil {
		t.Fatalf("an out-of-order migration must still apply: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "applied out of order") {
		t.Fatalf("warnings = %v, want one out-of-order report", warnings)
	}
	var exists bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='control' AND table_name='claim' AND column_name='probe_late')`).Scan(&exists); err != nil || !exists {
		t.Fatalf("the out-of-order migration was not applied (exists=%v, err=%v)", exists, err)
	}
	warnings = nil
	ahead := append(slices.Clone(late), probe(last.Version+1, "probe_ahead"))
	if err := (Runner{Direct: pool, Catalog: ahead, BackupRef: "test", InstanceID: "migration", Holder: "test", Report: report}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("a migration above the applied maximum warned: %v", warnings)
	}
}
