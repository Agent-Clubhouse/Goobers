package schema_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/schema"
)

var (
	blobIsReferencedDefinition = regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+control\.blob_is_referenced\s*\(.*?\$\$(.*?)\$\$`)
	blobColumnProbe            = regexp.MustCompile(`(?is)control\.blob_column_referenced\(\s*'([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)'\s*,\s*'([a-z_][a-z0-9_]*)'`)
	createdTable               = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)`)
)

type blobProbe struct{ schema, table, column string }

// blobReferenceProbes returns the columns the latest definition of
// control.blob_is_referenced probes, and the tables the catalog creates.
func blobReferenceProbes(t *testing.T) ([]blobProbe, map[string]bool) {
	t.Helper()
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var body string
	created := make(map[string]bool)
	for _, migration := range catalog {
		for _, match := range blobIsReferencedDefinition.FindAllStringSubmatch(migration.SQL, -1) {
			body = match[1]
		}
		for _, match := range createdTable.FindAllStringSubmatch(migration.SQL, -1) {
			created[strings.ToLower(match[1]+"."+match[2])] = true
		}
	}
	if body == "" {
		t.Fatal("no migration defines control.blob_is_referenced")
	}
	var probes []blobProbe
	for _, match := range blobColumnProbe.FindAllStringSubmatch(body, -1) {
		probes = append(probes, blobProbe{schema: match[1], table: match[2], column: match[3]})
	}
	return probes, created
}

// TestBlobReferenceProbesAreControllerReadable keeps the SECURITY INVOKER
// blob reference check runnable as goobers_controller. Every probed column of a
// table the catalog creates must be in the controller's write-path readers (so
// it gets SELECT on the column and USAGE on the schema). A table that lands
// without that row would otherwise make config prune and blob GC fail with
// "permission denied" instead of treating the digest as referenced.
func TestBlobReferenceProbesAreControllerReadable(t *testing.T) {
	probes, created := blobReferenceProbes(t)
	if len(probes) == 0 {
		t.Fatal("found no blob_column_referenced probes in control.blob_is_referenced")
	}
	inventory, err := schema.LoadWritePaths()
	if err != nil {
		t.Fatal(err)
	}
	readable := make(map[string]bool)
	for _, table := range inventory.Tables {
		if !table.Provisioned {
			continue
		}
		for _, reader := range table.Readers {
			if reader.Role != "goobers_controller" {
				continue
			}
			for _, column := range reader.Columns {
				readable[table.Schema+"."+table.Name+"."+column] = true
			}
		}
	}
	checked := 0
	for _, probe := range probes {
		qualified := probe.schema + "." + probe.table
		if !created[qualified] {
			continue // blob_column_referenced fails closed for a table that does not exist
		}
		checked++
		if !readable[qualified+"."+probe.column] {
			t.Errorf("control.blob_is_referenced probes %s.%s, but writepaths.yaml does not give goobers_controller SELECT on it", qualified, probe.column)
		}
	}
	if checked == 0 {
		t.Fatal("no probed table exists in the catalog; the guard checked nothing")
	}
}

// TestBlobReferenceProbeParserSeesKnownProbes keeps the parser honest: the
// probes migration 0020 is known to make must all be found.
func TestBlobReferenceProbeParserSeesKnownProbes(t *testing.T) {
	probes, _ := blobReferenceProbes(t)
	for _, want := range []blobProbe{
		{"control", "config_generation", "blob_digest"},
		{"control", "recovery_record", "patch_digest"},
		{"control", "recovery_record", "archive_digest"},
		{"journal", "run_artifact", "digest"},
		{"control", "child_lineage", "workspace_ref"},
		{"control", "child_disposition_history", "result_ref"},
	} {
		if !slices.Contains(probes, want) {
			t.Errorf("parser missed probe %+v (found %d probes)", want, len(probes))
		}
	}
}
