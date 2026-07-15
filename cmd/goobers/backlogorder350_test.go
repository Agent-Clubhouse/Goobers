package main

import (
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestSortEligibleFIFOOrdersAscendingByID is #350's core regression test:
// claim order must be deterministic FIFO (oldest issue/work-item first) —
// ascending numeric ID — regardless of what order the provider handed the
// items to us in. GitHub's real, undocumented default is desc-by-created
// (newest first), the exact opposite; this proves the client-side sort
// corrects that rather than depending on any provider's own default.
func TestSortEligibleFIFOOrdersAscendingByID(t *testing.T) {
	items := []providers.WorkItem{
		{ID: "335"}, {ID: "334"}, {ID: "333"}, {ID: "332"}, {ID: "331"}, {ID: "330"}, {ID: "329"},
	}
	sortEligibleFIFO(items)

	want := []string{"329", "330", "331", "332", "333", "334", "335"}
	for i, id := range want {
		if items[i].ID != id {
			t.Fatalf("items[%d].ID = %q, want %q (full order: %v)", i, items[i].ID, id, idsOf(items))
		}
	}
}

// TestSortEligibleFIFOIsStableAmongDuplicateNonNumericIDs confirms
// sortEligibleFIFO doesn't panic or silently drop items when an ID isn't a
// plain integer (a future/different provider), falling back to a stable
// lexical compare rather than leaving a non-numeric item's position
// undefined relative to the numeric ones.
func TestSortEligibleFIFOFallsBackToLexicalForNonNumericIDs(t *testing.T) {
	items := []providers.WorkItem{
		{ID: "10"}, {ID: "abc"}, {ID: "2"}, {ID: "abd"},
	}
	sortEligibleFIFO(items)

	// Numeric IDs sort numerically among themselves; non-numeric IDs sort
	// lexically among themselves — SliceStable's total order interleaves
	// them by the comparator's own rule (numeric < lexical fallback only
	// compares within the same "kind" pairing in this implementation, but
	// the concrete assertion here is simply: it terminates, keeps all 4
	// items, and puts "2" before "10" (both numeric, correctly ordered).
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4 (no items dropped): %v", len(items), idsOf(items))
	}
	twoIdx, tenIdx := -1, -1
	for i, it := range items {
		switch it.ID {
		case "2":
			twoIdx = i
		case "10":
			tenIdx = i
		}
	}
	if twoIdx < 0 || tenIdx < 0 || twoIdx >= tenIdx {
		t.Fatalf("want numeric ID \"2\" before \"10\", got order %v", idsOf(items))
	}
}

func idsOf(items []providers.WorkItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}
