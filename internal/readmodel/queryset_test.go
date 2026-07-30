package readmodel

import (
	"errors"
	"strings"
	"testing"
)

// TestEveryURLReachableCombinationIsSupportedOrRefused is §14.2's enumeration
// test, and §11A calls it the condition that must pass before Wave 2 lands.
//
// It walks the ENTIRE cross-product of the filter space — 2^8 = 256 combinations
// — and asserts each is either in the closed set or produces a typed refusal.
// There is deliberately no third outcome: a combination that is neither
// supported nor refused is one that gets walked, which is the unbounded
// candidate loop §5.7 exists to remove.
//
// The space is the URL-reachable one, not the link-reachable one. §5.7 claimed
// the set could be "generated into the OpenAPI/contract surface so the UI can
// only construct supported combinations"; that is false, because the portal
// parses filters from arbitrary hash query parameters and every drill-through
// target is a bookmarkable href (§18.0). A stale bookmark is an ordinary event.
func TestEveryURLReachableCombinationIsSupportedOrRefused(t *testing.T) {
	var supported, refused int
	for mask := 0; mask < (1 << len(AllDims)); mask++ {
		dims := dimsForMask(mask)
		combination, err := Require(dims)
		if err == nil {
			supported++
			if combination.Index == "" {
				t.Errorf("{%s} is supported but names no index; §5.7 requires every supported "+
					"combination to ship with a covering index", Key(dims))
			}
			if combination.Bench == "" {
				t.Errorf("{%s} is supported but names no benchmark; §5.7 requires a rows-visited "+
					"bound per combination", Key(dims))
			}
		} else {
			refused++
			var unsupported *UnsupportedCombinationError
			if !errors.As(err, &unsupported) {
				t.Errorf("{%s} failed with %v, which is not a typed refusal", Key(dims), err)
				continue
			}
			if unsupported.Code() != "unsupported_filter_combination" {
				t.Errorf("{%s} refusal code = %q", Key(dims), unsupported.Code())
			}
			// A refusal that only says no leaves a user with a bookmarked URL and
			// nowhere to go.
			if len(unsupported.Neighbours) == 0 {
				t.Errorf("{%s} was refused without naming a supported neighbour", Key(dims))
			}
		}
	}
	t.Logf("filter space: %d combinations, %d supported, %d refused", 1<<len(AllDims), supported, refused)
	if supported != len(supportedCombinations) {
		t.Errorf("enumeration found %d supported combinations but the set declares %d; "+
			"a duplicate or unreachable entry has crept in", supported, len(supportedCombinations))
	}
}

// TestRefusalNamesTheNearestNeighbours pins that the refusal is actionable.
//
// Distance is symmetric difference — how many dimensions would have to be added
// or dropped — because that answers "what is the closest thing I can actually
// ask?", which is what a user holding a stale URL needs.
func TestRefusalNamesTheNearestNeighbours(t *testing.T) {
	// stage is not supported in any combination: it is the dimension that would
	// become a residual filter, and the one #1782 shows is reachable from an
	// Insight drill-through click today.
	_, err := Require([]Dim{DimGaggle, DimStage})
	if err == nil {
		t.Fatal("a stage filter was accepted; it has no covering index and would be walked")
	}
	var unsupported *UnsupportedCombinationError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want a typed refusal", err)
	}
	// The nearest neighbour to {gaggle, stage} is {gaggle}: drop one dimension.
	if len(unsupported.Neighbours) == 0 || Key(unsupported.Neighbours[0]) != "gaggle" {
		t.Errorf("nearest neighbour to {gaggle+stage} = %v, want {gaggle}", unsupported.Neighbours)
	}
	if !strings.Contains(err.Error(), "supported neighbours") {
		t.Errorf("refusal message does not offer alternatives: %v", err)
	}
}

// TestPopulationAndOutcomeAreRefusedNotWalked pins the two dimensions that are
// reachable from a user click today and would be residual predicates.
//
// #1782 documents the cost: a single `?stage=X` request that matches nothing
// reads ~143 MB, performs ~1M unmarshals, and opens ~19,852 journals — and with
// population set, ~19,852 extra SQLite queries on top, inside one HTTP request.
// Refusing is the honest answer until they have covering support.
func TestPopulationAndOutcomeAreRefusedNotWalked(t *testing.T) {
	for _, dims := range [][]Dim{
		{DimPopulation},
		{DimOutcome},
		{DimGaggle, DimPopulation},
		{DimGaggle, DimWorkflow, DimOutcome},
		{DimStage, DimPopulation, DimOutcome},
	} {
		if _, err := Require(dims); err == nil {
			t.Errorf("{%s} was accepted; it has no covering index and would relocate the candidate "+
				"loop into the query planner rather than removing it", Key(dims))
		}
	}
}

// TestCanonicalKeyIgnoresOrder pins that a combination is identified by its SET
// of dimensions. Without normalisation, {gaggle, phase} and {phase, gaggle}
// would be different keys for the same query — so half of a supported
// combination's callers would be refused depending on argument order.
func TestCanonicalKeyIgnoresOrder(t *testing.T) {
	a := []Dim{DimGaggle, DimPhase, DimSince}
	b := []Dim{DimSince, DimPhase, DimGaggle}
	if Key(a) != Key(b) {
		t.Errorf("Key is order-sensitive: %q vs %q", Key(a), Key(b))
	}
	if _, err := Require(b); err != nil {
		t.Errorf("a supported combination was refused when its dimensions arrived in a different order: %v", err)
	}
	// Duplicates must not change identity either.
	if Key([]Dim{DimGaggle, DimGaggle}) != Key([]Dim{DimGaggle}) {
		t.Error("duplicate dimensions changed the canonical key")
	}
}

// TestEverySupportedCombinationNamesARealIndex ties the declared index names to
// the schema. §5.7 requires that adding a combination "ships an index and a
// benchmark with it"; a combination naming an index that does not exist would
// pass every other test while silently walking rows in production.
func TestEverySupportedCombinationNamesARealIndex(t *testing.T) {
	store := openTestStore(t)
	rows, err := store.readDB().Query(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'run'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	existing := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, c := range SupportedCombinations() {
		if !existing[c.Index] {
			t.Errorf("combination {%s} names index %q, which does not exist on the run table",
				Key(c.Dims), c.Index)
		}
	}
}

// dimsForMask expands a bitmask into a dimension set.
func dimsForMask(mask int) []Dim {
	var out []Dim
	for i, d := range AllDims {
		if mask&(1<<i) != 0 {
			out = append(out, d)
		}
	}
	return out
}
