package validate

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// #1677: `BacklogRef.Query` was a documented, serializable DSL field that no
// provider ever read, so the schema advertised a selection capability that
// silently did nothing. The resolution taken was removal (#3320, pre-tag, zero
// readers) — and the acceptance criterion that outlives the deletion is that
// the contract "must not silently accept an ineffective query". These tests
// are what enforce that: a config still carrying the field fails to load, and
// says what replaced it.
func TestBacklogQueryIsRejectedWithItsRemovalExplained(t *testing.T) {
	gaggle := strings.Replace(
		validGaggleYAML(),
		"    project: example/web",
		"    project: example/web\n    query: \"is:open label:bug\"",
		1,
	)
	config := schemaPositionConfig(gaggle, "")
	report := writeAndValidate(t, config)
	if !report.HasErrors() {
		t.Fatal("a gaggle declaring backlog.query must fail validation, not be silently ignored")
	}

	var found *Issue
	for i := range report.Issues {
		if report.Issues[i].Code == errorSchemaViolation && strings.Contains(report.Issues[i].Message, "query") {
			found = &report.Issues[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no schema violation naming query:\n%s", joinIssues(report))
	}
	for _, want := range []string{"#1677", "labelPredicate", "fieldPredicate", "`labels`"} {
		if !strings.Contains(found.Message, want) {
			t.Errorf("message = %q, want it to mention %s", found.Message, want)
		}
	}
	// A retired field is not a typo. Offering the nearest surviving field name
	// would send the author at whichever one happens to be closest in
	// spelling instead of at the fields that actually replaced it.
	if strings.Contains(found.Message, "did you mean") {
		t.Errorf("a retired field must not get a near-miss suggestion: %q", found.Message)
	}
	// The finding must land on the author's own line, so the fix needs no
	// further searching.
	wantLine := strings.Count(config[:strings.Index(config, "query:")], "\n") + 1
	if found.Line != wantLine {
		t.Errorf("line = %d, want %d (the query: key's own line)", found.Line, wantLine)
	}
}

// An ordinary typo in the same object still gets the near-miss suggestion:
// the retired-field hint is an extra arm, not a replacement for it.
func TestBacklogTypoStillGetsNearMissSuggestion(t *testing.T) {
	gaggle := strings.Replace(
		validGaggleYAML(),
		"    project: example/web",
		"    project: example/web\n    labelPredicat: \"x\"",
		1,
	)
	report := writeAndValidate(t, schemaPositionConfig(gaggle, ""))
	got := joinIssues(report)
	if !strings.Contains(got, `did you mean "labelPredicate"?`) {
		t.Fatalf("want a near-miss suggestion for labelPredicat:\n%s", got)
	}
}

// The two halves of the removal must stay removed together. A field that
// exists in the Go type but not the schema is accepted by the runtime loader
// and rejected by `validate`; one that exists in the schema but not the type
// is validated and then silently dropped — which is the exact shape of the
// defect #1677 reported.
func TestBacklogRefSchemaAndTypeAgreeOnTheSelectionSurface(t *testing.T) {
	schemaFields := backlogRefSchemaProperties(t)
	typeFields := yamlFieldNames(reflect.TypeOf(apiv1.BacklogRef{}))
	if !reflect.DeepEqual(schemaFields, typeFields) {
		t.Fatalf("gaggle schema backlogRef properties = %v, BacklogRef yaml fields = %v", schemaFields, typeFields)
	}
	for _, name := range typeFields {
		if name == "query" {
			t.Fatal("BacklogRef declares `query` again; a field no provider consumes advertises a capability that does nothing (#1677)")
		}
	}
}

func yamlFieldNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// backlogRefSchemaProperties reads the shipped gaggle schema's backlogRef
// definition rather than a copy of it, so this test cannot pass against a
// contract the CLI does not actually enforce.
func backlogRefSchemaProperties(t *testing.T) []string {
	t.Helper()
	raw, err := schemas.FS.ReadFile(schemas.Kind["Gaggle"])
	if err != nil {
		t.Fatalf("read gaggle schema: %v", err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode gaggle schema: %v", err)
	}
	def, ok := doc.Defs["backlogRef"]
	if !ok {
		t.Fatal("gaggle schema has no $defs/backlogRef")
	}
	names := make([]string, 0, len(def.Properties))
	for name := range def.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
