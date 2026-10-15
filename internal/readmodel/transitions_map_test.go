package readmodel

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// TestRetryBackoffMarksMapCoverageParallel pins that a map visit, like a
// parallel, marks retry coverage as incomplete before any item event lands.
func TestRetryBackoffMarksMapCoverageParallel(t *testing.T) {
	state := RetryBackoffState{}.After(tev(1, journal.EventMapStarted, func(e *journal.Event) { e.Parallel = "run-shards" }))
	if !state.Parallel {
		t.Fatal("map.started did not mark parallel retry coverage")
	}
}

// TestProjectTransitionsMapIsAGraphNode pins that a map, like a parallel, is a
// graph node the root branch passes through: plan -> map on map.started and
// map -> join after map.finished, while each item branch settles on its own
// timeline without cross-item edges.
func TestProjectTransitionsMapIsAGraphNode(t *testing.T) {
	item := func(seq uint64, branch int, kind journal.EventType, identity string) journal.Event {
		return tev(seq, kind, func(e *journal.Event) {
			e.Branch, e.Parallel, e.BranchName, e.MapItemIdentity = branch, "run-shards", identity, identity
			if kind == journal.EventBranchFinished {
				e.BranchStatus = journal.BranchSucceeded
			}
		})
	}
	events := []journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "plan" }),
		tev(2, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "plan", "success" }),
		tev(3, journal.EventMapStarted, func(e *journal.Event) {
			e.Parallel = "run-shards"
			e.MapRoster = &journal.MapRoster{Items: []journal.MapItem{{Identity: "unit-a"}, {Identity: "unit-b"}}}
		}),
		item(4, 1, journal.EventBranchStarted, "unit-a"),
		tev(5, journal.EventStageStarted, func(e *journal.Event) { e.Branch, e.Stage = 1, "run-shard" }),
		tev(6, journal.EventStageFinished, func(e *journal.Event) { e.Branch, e.Stage, e.Status = 1, "run-shard", "success" }),
		item(7, 1, journal.EventBranchFinished, "unit-a"),
		item(8, 2, journal.EventBranchStarted, "unit-b"),
		tev(9, journal.EventStageStarted, func(e *journal.Event) { e.Branch, e.Stage = 2, "run-shard" }),
		tev(10, journal.EventStageFinished, func(e *journal.Event) { e.Branch, e.Stage, e.Status = 2, "run-shard", "success" }),
		item(11, 2, journal.EventBranchFinished, "unit-b"),
		tev(12, journal.EventMapFinished, func(e *journal.Event) { e.Parallel = "run-shards" }),
		tev(13, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "collate" }),
		tev(14, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "collate", "success" }),
		tev(15, journal.EventRunFinished, func(e *journal.Event) { e.Status = string(journal.PhaseCompleted) }),
	}
	graph := &workflow.Graph{
		Name: "mapped", Version: 1, Digest: "d1", Start: "plan",
		Nodes: []workflow.GraphNode{
			{ID: "plan", Kind: workflow.GraphNodeAgentic},
			{ID: "run-shard", Kind: workflow.GraphNodeDeterministic},
			{ID: "collate", Kind: workflow.GraphNodeAgentic},
			{ID: "run-shards", Kind: workflow.GraphNodeMap},
		},
		Edges: []workflow.GraphEdge{
			{Source: "plan", Target: "run-shards"},
			{Source: "run-shard", Target: "collate"},
			{Source: "collate", Terminal: workflow.GraphTerminalComplete},
			{Source: "run-shards", Target: "run-shard"},
		},
	}
	rows, status := ProjectTransitions(events, graph)
	if status != TransitionsProjected {
		t.Fatalf("status = %q, want %q", status, TransitionsProjected)
	}
	byBranch := map[int][]TransitionRow{}
	for _, row := range rows {
		byBranch[row.Branch] = append(byBranch[row.Branch], row)
	}
	assertRows(t, byBranch[0], []TransitionRow{
		{Branch: 0, Seq: 3, Source: "plan", Target: "run-shards"},
		{Branch: 0, Seq: 13, Source: "run-shards", Target: "collate"},
		{Branch: 0, Seq: 15, Source: "collate", Terminal: true, TerminalStatus: "completed"},
	})
	for _, branch := range []int{1, 2} {
		assertRows(t, byBranch[branch], []TransitionRow{
			{Branch: branch, Seq: map[int]uint64{1: 7, 2: 11}[branch], Source: "run-shard", Terminal: true, TerminalStatus: "succeeded"},
		})
	}
}
