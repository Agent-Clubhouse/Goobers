package readservice

import (
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/readmodel"
)

// TestReadServiceCannotWriteProjectedFacts is #1922's compile-time acceptance
// criterion: "readservice cannot obtain a writable handle to projected facts."
//
// # Why this is written against the DECLARED type
//
// The obvious version of this test is a type assertion on the field's value:
//
//	if _, ok := any(sources.ReadModel).(readmodel.Writer); ok { … }
//
// That is vacuous. `sources.ReadModel` on a zero LocalSources is a nil
// interface, and a nil interface fails every assertion — so the test passes
// regardless of the field's type. Populate it with a real *readmodel.Store and
// it inverts: the concrete store DOES implement Writer, so the assertion fires
// even though the seam is intact.
//
// The guarantee is not about any value. It is about the METHOD SET of the type
// the field is declared as, which is what decides whether
// `s.sources.ReadModel.UpsertRun(...)` compiles. So that is what this inspects.
//
// # Why the seam matters more than it sounds
//
// A read path writing to disk is not hypothetical — it is the defect §6.3 exists
// to remove. `reconcileIndex` runs on the HTTP list path and reaches
// `IngestRun` → `WithPruneProtection` → `acquireJournalLock`, which is why all
// 40,665 run directories on the live instance contain a `.lock` file, including
// the 10,906 with no `run.yaml` that can never be ingested. Every one was
// created by a read. It looked like an index refresh, so a review rule would not
// have caught it. A type does.
func TestReadServiceCannotWriteProjectedFacts(t *testing.T) {
	field, ok := reflect.TypeOf(LocalSources{}).FieldByName("ReadModel")
	if !ok {
		t.Fatal("LocalSources has no ReadModel field; this test needs updating alongside " +
			"whatever replaced it, not deleting")
	}
	if field.Type.Kind() != reflect.Interface {
		t.Fatalf("LocalSources.ReadModel is declared as %s, a concrete type. Every method on "+
			"it is reachable from the read path, including projection and repair. It must be "+
			"an interface that carries reads only.", field.Type)
	}

	// The write surfaces, named individually so a failure says which one leaked
	// rather than just "too wide".
	forbidden := []string{
		"UpsertRun",    // projection
		"PruneChanges", // retention
		"Observed",     // intake watermark
		"Removing",     // retention intent
		"BuildFromJournals",
		"ProjectRunDir",
		"Close", // lifecycle belongs to whoever opened it
	}
	for _, name := range forbidden {
		if _, found := field.Type.MethodByName(name); found {
			t.Errorf("LocalSources.ReadModel (%s) exposes %s to the read path; §3.1 requires "+
				"that a read cannot write, backfill, or repair", field.Type, name)
		}
	}

	// And the reads it does need are present, so the seam is narrow rather than
	// merely empty — a handle carrying nothing would also pass the checks above
	// while being useless.
	for _, name := range []string{"ListRuns", "LatestPerWorkflow", "ActiveRunCounts", "GetRun", "State"} {
		if _, found := field.Type.MethodByName(name); !found {
			t.Errorf("LocalSources.ReadModel (%s) is missing %s, which the read paths use",
				field.Type, name)
		}
	}
}

// TestTheThreeCapabilitiesAreDisjointWhereItMatters pins that Reader carries no
// method belonging to the other two capabilities.
//
// §3.1 splits three capabilities — read, project, record-intake — precisely so a
// component can hold one without the others. If Reader ever grew a write method,
// the split would still LOOK intact (three named interfaces) while providing
// nothing, and the test above would keep passing as long as the specific names
// it lists stayed absent. This checks the property rather than a list.
func TestTheThreeCapabilitiesAreDisjointWhereItMatters(t *testing.T) {
	readerType := reflect.TypeOf((*readmodel.Reader)(nil)).Elem()
	for _, other := range []struct {
		name string
		typ  reflect.Type
	}{
		{"Writer", reflect.TypeOf((*readmodel.Writer)(nil)).Elem()},
		{"Intake", reflect.TypeOf((*readmodel.Intake)(nil)).Elem()},
	} {
		for i := 0; i < other.typ.NumMethod(); i++ {
			method := other.typ.Method(i).Name
			if _, found := readerType.MethodByName(method); found {
				t.Errorf("readmodel.Reader carries %s, which belongs to %s; the capability "+
					"split is nominal rather than real", method, other.name)
			}
		}
	}
}
