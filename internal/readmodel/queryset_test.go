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

func TestRefusalNamesTheNearestNeighbours(t *testing.T) {
	// workflow+stage is the refusal that survives #1782. Stage-scoped queries
	// drive from run_stage, and workflow is NOT denormalised onto it -- so
	// combining them would put workflow back as a residual predicate on the
	// joined run row, which is the shape the closed set exists to refuse.
	_, err := Require([]Dim{DimGaggle, DimWorkflow, DimStage})
	if err == nil {
		t.Fatal("workflow+stage was accepted; workflow is not on run_stage, so it would be walked")
	}
	var unsupported *UnsupportedCombinationError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want a typed refusal", err)
	}
	// Nearest neighbours are one dimension away: drop workflow, or drop stage.
	if len(unsupported.Neighbours) == 0 {
		t.Fatal("refusal offered no neighbours")
	}
	nearest := Key(unsupported.Neighbours[0])
	if nearest != "gaggle+workflow" && nearest != "gaggle+stage" {
		t.Errorf("nearest neighbour to {gaggle+workflow+stage} = %q, want one of "+
			"{gaggle+workflow} or {gaggle+stage}", nearest)
	}
	if !strings.Contains(err.Error(), "supported neighbours") {
		t.Errorf("refusal message does not offer alternatives: %v", err)
	}
}

// TestStageAndPopulationAreServedNotWalked is the inverse of the test it
// replaces (#1782).
//
// That test pinned stage, outcome, and population as REFUSED, on the reasoning
// that refusing was more honest than walking: a single `?stage=X` request that
// matched nothing read ~143 MB, performed ~1M unmarshals, and opened ~19,852
// journals -- with population set, ~19,852 extra SQLite queries on top, inside
// one HTTP request.
//
// They are served now because they have covering support: the stage predicate,
// the gaggle scope, and the run-recency ordering all live on run_stage, and the
// four populations are partial indexes at both grains. The old assertion is
// inverted rather than deleted, so the property it protected -- that these are
// never merely WALKED -- is still what the test is about.
func TestStageOutcomeAndPopulationAreServedNotWalked(t *testing.T) {
	for _, dims := range [][]Dim{
		{DimStage},
		{DimGaggle, DimStage},
		{DimStage, DimOutcome},
		{DimGaggle, DimStage, DimOutcome, DimSince, DimUntil},
		{DimStage, DimPopulation},
		{DimPopulation},
		{DimGaggle, DimPopulation},
		{DimGaggle, DimStage, DimPopulation, DimSince, DimUntil},
	} {
		if _, err := Require(dims); err != nil {
			t.Errorf("{%s} was refused; #1782 gives it a covering index: %v", Key(dims), err)
		}
	}
}

// TestCombinationsWithoutCoveringSupportAreStillRefused pins what #1782 did NOT
// make servable, so the closed set does not quietly become open.
func TestCombinationsWithoutCoveringSupportAreStillRefused(t *testing.T) {
	for _, c := range []struct {
		dims []Dim
		why  string
	}{
		{[]Dim{DimOutcome}, "run-level outcome has no recency index of its own"},
		{[]Dim{DimGaggle, DimWorkflow, DimOutcome}, "same, and workflow is not on run_stage"},
		{[]Dim{DimStage, DimPopulation, DimOutcome}, "no index leads with all three"},
		{[]Dim{DimWorkflow, DimStage}, "workflow is not denormalised onto run_stage"},
		{[]Dim{DimPhase, DimStage}, "phase is not on run_stage either"},
	} {
		if _, err := Require(c.dims); err == nil {
			t.Errorf("{%s} was accepted, but %s", Key(c.dims), c.why)
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
	rows, err := store.reader.Query(
		`SELECT name, tbl_name FROM sqlite_master WHERE type = 'index' AND tbl_name IN ('run', 'run_stage')`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	// Both tables: stage-scoped combinations are served by indexes on run_stage,
	// which is the whole point of #1782 -- the stage predicate, the gaggle scope,
	// and the run-recency ordering have to live on one index, and only run_stage
	// carries all three.
	existing := map[string]string{}
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			t.Fatal(err)
		}
		existing[name] = table
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, c := range SupportedCombinations() {
		if existing[c.Index] == "" {
			t.Errorf("combination {%s} names index %q, which exists on neither run nor run_stage",
				Key(c.Dims), c.Index)
			continue
		}
		// A stage-scoped combination served by an index on `run` would be a
		// residual-predicate bug wearing a covering index's name, so the table is
		// asserted rather than just the name.
		wantTable := "run"
		if hasDim(c.Dims, DimStage) {
			wantTable = "run_stage"
		}
		if existing[c.Index] != wantTable {
			t.Errorf("combination {%s} names index %q on table %q, want %q",
				Key(c.Dims), c.Index, existing[c.Index], wantTable)
		}
	}
}

// hasDim reports whether a dimension set contains a dimension.
func hasDim(dims []Dim, want Dim) bool {
	for _, d := range dims {
		if d == want {
			return true
		}
	}
	return false
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
