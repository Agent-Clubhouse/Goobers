package dispatcher

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// TestPinnedPlacementJournalMirror pins journal.PinnedPlacement / PinnedRunner
// (run.yaml's spelling, which the daemon's runner carries) to PinnedPlacement
// / RunnerSpec (the dispatch wire type): every field round-trips in both
// directions, the JSON bytes are identical, and the field/tag sets match, so
// the two cannot drift apart.
func TestPinnedPlacementJournalMirror(t *testing.T) {
	full := PinnedPlacement{
		Stage: "build", Self: false, Queue: "q",
		Eligible: []RunnerSpec{{
			Name: "linux", OS: "linux", HostKind: instance.RunnerHostKind("image"), Host: "img:1",
			CPU: "2", Memory: "4Gi", Disk: "20Gi", Restrictions: []string{"no-network"},
		}},
		LedgerTouching: true, CPU: "500m", Memory: "1Gi", Disk: "1Gi", Restrictions: []string{"no-secrets"},
	}
	if restored := PinnedPlacementFromJournal(full.Journal()); !reflect.DeepEqual(restored, full) {
		t.Fatalf("field conversion lost data:\n got %+v\nwant %+v", restored, full)
	}
	want, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var viaMirror journal.PinnedPlacement
	if err := json.Unmarshal(want, &viaMirror); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(viaMirror)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("mirror JSON drifted from dispatcher JSON:\n got %s\nwant %s", got, want)
	}
	if !reflect.DeepEqual(viaMirror, full.Journal()) {
		t.Fatalf("JSON-decoded mirror %+v != converted mirror %+v", viaMirror, full.Journal())
	}
	fieldNames := func(v any) []string {
		rt := reflect.TypeOf(v)
		names := make([]string, 0, rt.NumField())
		for i := 0; i < rt.NumField(); i++ {
			names = append(names, rt.Field(i).Name+" "+string(rt.Field(i).Tag))
		}
		return names
	}
	if a, b := fieldNames(PinnedPlacement{}), fieldNames(journal.PinnedPlacement{}); !reflect.DeepEqual(a, b) {
		t.Fatalf("PinnedPlacement fields drifted:\n dispatcher %v\n journal    %v", a, b)
	}
	if a, b := fieldNames(RunnerSpec{}), fieldNames(journal.PinnedRunner{}); !reflect.DeepEqual(a, b) {
		t.Fatalf("RunnerSpec/PinnedRunner fields drifted:\n dispatcher %v\n journal    %v", a, b)
	}
	if PinnedPlacementsJournal(nil) != nil {
		t.Fatal("nil placements must convert to nil, so an unplaced run.yaml keeps its bytes")
	}
}
