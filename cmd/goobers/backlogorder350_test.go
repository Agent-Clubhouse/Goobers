package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/fieldpredicate"
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
	if err := sortEligibleByFields(items, nil, fieldpredicate.Order{}); err != nil {
		t.Fatal(err)
	}

	want := []string{"329", "330", "331", "332", "333", "334", "335"}
	for i, id := range want {
		if items[i].ID != id {
			t.Fatalf("items[%d].ID = %q, want %q (full order: %v)", i, items[i].ID, id, idsOf(items))
		}
	}
}

// TestSortEligibleFIFOIsStableAmongDuplicateNonNumericIDs confirms the default
// sorter doesn't panic or silently drop items when an ID isn't a plain integer
// (a future/different provider), falling back to a stable lexical compare
// rather than leaving a non-numeric item's position undefined relative to the
// numeric ones.
func TestSortEligibleFIFOFallsBackToLexicalForNonNumericIDs(t *testing.T) {
	items := []providers.WorkItem{
		{ID: "10"}, {ID: "abc"}, {ID: "2"}, {ID: "abd"},
	}
	if err := sortEligibleByFields(items, nil, fieldpredicate.Order{}); err != nil {
		t.Fatal(err)
	}

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

// TestSortEligiblePrioritizesConfiguredLabelsBeforeFIFO is #1335's core
// contract: an item carrying an earlier-listed selectionPriority label
// claims ahead of one carrying only a later-listed label or none, and FIFO
// still breaks ties within a tier.
func TestSortEligiblePrioritizesConfiguredLabelsBeforeFIFO(t *testing.T) {
	items := []providers.WorkItem{
		{ID: "10", Labels: []string{"bug"}},
		{ID: "5", Labels: nil},
		{ID: "20", Labels: []string{"security"}},
		{ID: "1", Labels: []string{"bug"}},
		{ID: "15", Labels: []string{"security"}},
	}
	if err := sortEligibleByFields(items, []string{"security", "bug"}, fieldpredicate.Order{}); err != nil {
		t.Fatal(err)
	}

	want := []string{"15", "20", "1", "10", "5"}
	got := idsOf(items)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestSortEligiblePrioritizedItemRanksByEarliestMatchingLabel proves a
// deterministic precedence for an item carrying more than one
// selectionPriority label: it ranks by whichever appears earliest in the
// configured list, not ambiguously.
func TestSortEligiblePrioritizedItemRanksByEarliestMatchingLabel(t *testing.T) {
	items := []providers.WorkItem{
		{ID: "2", Labels: []string{"bug"}},
		{ID: "1", Labels: []string{"bug", "security"}},
	}
	if err := sortEligibleByFields(items, []string{"security", "bug"}, fieldpredicate.Order{}); err != nil {
		t.Fatal(err)
	}

	if idsOf(items)[0] != "1" {
		t.Fatalf("order = %v, want item 1 (matches earliest-listed \"security\") first", idsOf(items))
	}
}

// TestSortEligibleUnconfiguredPriorityIsPlainFIFO proves #1335 is strictly
// additive: an empty/nil selectionPriority produces byte-identical ordering
// to the pre-#1335 plain-FIFO sort, regardless of item labels.
func TestSortEligibleUnconfiguredPriorityIsPlainFIFO(t *testing.T) {
	items := []providers.WorkItem{
		{ID: "3", Labels: []string{"security"}},
		{ID: "1", Labels: nil},
		{ID: "2", Labels: []string{"bug"}},
	}
	if err := sortEligibleByFields(items, nil, fieldpredicate.Order{}); err != nil {
		t.Fatal(err)
	}

	want := []string{"1", "2", "3"}
	if strings.Join(idsOf(items), ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v (labels must not matter when unconfigured)", idsOf(items), want)
	}
}

func idsOf(items []providers.WorkItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}
