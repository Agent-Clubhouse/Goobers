package readmodel

import (
	"fmt"
	"sort"
	"strings"
)

// The closed set of supported filter combinations (#1920, design §5.7).
//
// # Why a closed set, rather than "make the filters fast"
//
// §5.7's central finding: **`limit+1` returned rows does not bound rows
// examined**, and neither does an index appearing in the plan. A selective
// residual predicate lets SQLite walk many recency candidates before it finds 51
// matches — which is the current candidate loop relocated into the query
// planner, not removed.
//
// And `EXPLAIN QUERY PLAN` **cannot prove a residual predicate is absent**.
// SQLite reports `SEARCH run USING INDEX idx_ab (a=?)` while silently evaluating
// `c=?` as a residual filter; the plan output does not enumerate residual terms.
// So the bound cannot come from a plan property. It comes from a finite,
// declared set: every supported combination ships with a covering index, and
// anything unlisted is a typed refusal.
//
// # The set is closed over URL-REACHABLE combinations, not link-reachable ones
//
// This is a correction to the design, recorded in §18.0. §5.7 says the set is
// "generated into the OpenAPI/contract surface so the UI can only construct
// supported combinations". That is false: the portal parses its filters from
// **arbitrary hash query parameters** (`portal/src/routing.ts`), validating enum
// membership but not which *combinations* are legal. Every Insight drill-through
// target is a bookmarkable `#/runs?…` href, so users share and re-enter these
// URLs — and one value, `population=premium-measured`, is reachable ONLY by URL
// and produced by no UI element at all.
//
// So the enumeration below must cover what the router can parse, and the
// refusal must degrade gracefully in the client, because a stale bookmark is a
// normal event rather than a misuse.

// Dim is one filter dimension a list query can constrain.
type Dim string

// The dimensions the run list can be filtered by. These are exactly what
// portal/src/routing.ts can parse from a URL, plus phase, which the Overview
// supplies directly.
const (
	DimGaggle     Dim = "gaggle"
	DimWorkflow   Dim = "workflow"
	DimPhase      Dim = "phase"
	DimStage      Dim = "stage"
	DimOutcome    Dim = "outcome"
	DimPopulation Dim = "population"
	DimSince      Dim = "since"
	DimUntil      Dim = "until"
)

// AllDims is the full filter space, in canonical order.
//
// Canonical order matters: a combination is identified by its SET of dimensions,
// so every lookup normalises to this order. Without it, {gaggle, phase} and
// {phase, gaggle} would be different keys for the same query.
var AllDims = []Dim{
	DimGaggle, DimWorkflow, DimPhase, DimStage, DimOutcome, DimPopulation, DimSince, DimUntil,
}

// Combination is one supported filter combination.
//
// Index and Bench are not documentation. §5.7 requires that "adding a
// combination is a deliberate act that ships an index and a benchmark with it",
// and naming them here is what makes that checkable — a combination whose index
// does not exist fails a test rather than silently walking rows.
type Combination struct {
	Dims  []Dim
	Index string
	Bench string
}

// Key returns the canonical identity of a dimension set.
func Key(dims []Dim) string {
	normalised := normalise(dims)
	parts := make([]string, len(normalised))
	for i, d := range normalised {
		parts[i] = string(d)
	}
	return strings.Join(parts, "+")
}

// normalise sorts dimensions into canonical order and removes duplicates.
func normalise(dims []Dim) []Dim {
	seen := make(map[Dim]struct{}, len(dims))
	out := make([]Dim, 0, len(dims))
	for _, want := range AllDims {
		for _, have := range dims {
			if have != want {
				continue
			}
			if _, dup := seen[want]; dup {
				continue
			}
			seen[want] = struct{}{}
			out = append(out, want)
		}
	}
	return out
}

// supportedCombinations is the closed set.
//
// Time bounds (since/until) are deliberately absorbed into the combinations that
// carry them rather than treated as a separate dimension: they are a range
// predicate on started_at, which every ordering index already leads with after
// its equality columns, so adding them does not change the access shape. The
// same is not true of stage, outcome, or population — those are the ones that
// would become residual filters, which is why they are listed explicitly and
// why the unlisted ones are refused rather than walked.
var supportedCombinations = []Combination{
	// Unrestricted recency — the Runs page with no filters, and the fast path
	// for a principal provably scoped to every gaggle (§5.5).
	{Dims: nil, Index: "idx_run_recency", Bench: "list/unrestricted"},
	{Dims: []Dim{DimSince}, Index: "idx_run_recency", Bench: "list/since"},
	{Dims: []Dim{DimUntil}, Index: "idx_run_recency", Bench: "list/until"},
	{Dims: []Dim{DimSince, DimUntil}, Index: "idx_run_recency", Bench: "list/window"},

	// Phase-scoped, which is what the Overview fans out over.
	{Dims: []Dim{DimPhase}, Index: "idx_run_phase_recency", Bench: "list/phase"},
	{Dims: []Dim{DimPhase, DimSince}, Index: "idx_run_phase_recency", Bench: "list/phase+since"},
	{Dims: []Dim{DimPhase, DimUntil}, Index: "idx_run_phase_recency", Bench: "list/phase+until"},
	{Dims: []Dim{DimPhase, DimSince, DimUntil}, Index: "idx_run_phase_recency", Bench: "list/phase+window"},

	// Gaggle-scoped. gaggle leads so authorization is a query predicate rather
	// than a post-filter (§5.5).
	{Dims: []Dim{DimGaggle}, Index: "idx_run_gaggle_recency", Bench: "list/gaggle"},
	{Dims: []Dim{DimGaggle, DimSince}, Index: "idx_run_gaggle_recency", Bench: "list/gaggle+since"},
	{Dims: []Dim{DimGaggle, DimUntil}, Index: "idx_run_gaggle_recency", Bench: "list/gaggle+until"},
	{Dims: []Dim{DimGaggle, DimSince, DimUntil}, Index: "idx_run_gaggle_recency", Bench: "list/gaggle+window"},

	{Dims: []Dim{DimGaggle, DimPhase}, Index: "idx_run_gaggle_phase_recency", Bench: "list/gaggle+phase"},
	{Dims: []Dim{DimGaggle, DimPhase, DimSince}, Index: "idx_run_gaggle_phase_recency", Bench: "list/gaggle+phase+since"},
	{Dims: []Dim{DimGaggle, DimPhase, DimUntil}, Index: "idx_run_gaggle_phase_recency", Bench: "list/gaggle+phase+until"},
	{Dims: []Dim{DimGaggle, DimPhase, DimSince, DimUntil}, Index: "idx_run_gaggle_phase_recency", Bench: "list/gaggle+phase+window"},

	// Gaggle + workflow, the workflow detail page's run history.
	{Dims: []Dim{DimGaggle, DimWorkflow}, Index: "idx_run_gaggle_workflow_recency", Bench: "list/gaggle+workflow"},
	{Dims: []Dim{DimGaggle, DimWorkflow, DimSince}, Index: "idx_run_gaggle_workflow_recency", Bench: "list/gaggle+workflow+since"},
	{Dims: []Dim{DimGaggle, DimWorkflow, DimUntil}, Index: "idx_run_gaggle_workflow_recency", Bench: "list/gaggle+workflow+until"},
	{Dims: []Dim{DimGaggle, DimWorkflow, DimSince, DimUntil}, Index: "idx_run_gaggle_workflow_recency", Bench: "list/gaggle+workflow+window"},
}

// Lookup returns the supported combination for a dimension set.
func Lookup(dims []Dim) (Combination, bool) {
	key := Key(dims)
	for _, c := range supportedCombinations {
		if Key(c.Dims) == key {
			return c, true
		}
	}
	return Combination{}, false
}

// SupportedCombinations returns the closed set, for contract generation and
// tests.
func SupportedCombinations() []Combination {
	out := make([]Combination, len(supportedCombinations))
	copy(out, supportedCombinations)
	return out
}

// UnsupportedCombinationError is the typed refusal.
//
// §5.7: "an unlisted combination returns a typed
// `unsupported_filter_combination` naming the supported neighbours, not a slow
// success". The neighbours matter — a refusal that only says no leaves a user
// with a bookmarked URL and nowhere to go, and this is reachable by an ordinary
// stale link rather than by misuse.
type UnsupportedCombinationError struct {
	Dims       []Dim
	Neighbours [][]Dim
}

// Code is the stable wire code for this refusal.
func (e *UnsupportedCombinationError) Code() string { return "unsupported_filter_combination" }

func (e *UnsupportedCombinationError) Error() string {
	if len(e.Neighbours) == 0 {
		return fmt.Sprintf("readmodel: unsupported filter combination {%s}", Key(e.Dims))
	}
	nearest := make([]string, 0, len(e.Neighbours))
	for _, n := range e.Neighbours {
		nearest = append(nearest, "{"+Key(n)+"}")
	}
	return fmt.Sprintf("readmodel: unsupported filter combination {%s}; supported neighbours: %s",
		Key(e.Dims), strings.Join(nearest, ", "))
}

// Require resolves a dimension set to a supported combination, or returns a
// typed refusal naming the nearest supported neighbours.
func Require(dims []Dim) (Combination, error) {
	if c, ok := Lookup(dims); ok {
		return c, nil
	}
	return Combination{}, &UnsupportedCombinationError{Dims: normalise(dims), Neighbours: neighbours(dims)}
}

// neighbours returns supported combinations closest to the requested one,
// nearest first.
//
// Distance is symmetric difference — how many dimensions would have to be added
// or dropped. That is the useful measure for a user holding a stale URL: it
// answers "what is the closest thing I can actually ask?", which is what makes
// the refusal actionable rather than merely correct.
func neighbours(dims []Dim) [][]Dim {
	want := make(map[Dim]struct{}, len(dims))
	for _, d := range normalise(dims) {
		want[d] = struct{}{}
	}
	type scored struct {
		dims     []Dim
		distance int
	}
	ranked := make([]scored, 0, len(supportedCombinations))
	for _, c := range supportedCombinations {
		have := make(map[Dim]struct{}, len(c.Dims))
		for _, d := range c.Dims {
			have[d] = struct{}{}
		}
		distance := 0
		for d := range want {
			if _, ok := have[d]; !ok {
				distance++
			}
		}
		for d := range have {
			if _, ok := want[d]; !ok {
				distance++
			}
		}
		ranked = append(ranked, scored{dims: c.Dims, distance: distance})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].distance != ranked[j].distance {
			return ranked[i].distance < ranked[j].distance
		}
		return Key(ranked[i].dims) < Key(ranked[j].dims)
	})
	out := make([][]Dim, 0, maxNeighbours)
	for _, r := range ranked {
		if len(out) == maxNeighbours {
			break
		}
		out = append(out, r.dims)
	}
	return out
}

// maxNeighbours bounds how many alternatives a refusal names. Enough to be
// useful, few enough to be readable in an error message or an empty state.
const maxNeighbours = 3
