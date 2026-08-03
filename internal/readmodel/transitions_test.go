package readmodel

import (
	"reflect"
	"sort"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// implementationGraph mirrors reference-workflows/gaggles/goobers/workflows/implementation.yaml's
// cyclic shape closely enough to exercise it: implement -> review, review
// pass -> local-ci, review needs-changes -> implement (a repass/back-edge),
// local-ci -> local-gate, local-gate pass -> open-pr, local-gate fail ->
// implement (a second back-edge), open-pr -> complete.
func implementationGraph() *workflow.Graph {
	return &workflow.Graph{
		Name: "implementation", Version: 1, Digest: "d1", Start: "implement",
		Nodes: []workflow.GraphNode{
			{ID: "implement", Kind: workflow.GraphNodeAgentic},
			{ID: "review", Kind: workflow.GraphNodeGate},
			{ID: "local-ci", Kind: workflow.GraphNodeDeterministic},
			{ID: "local-gate", Kind: workflow.GraphNodeGate},
			{ID: "open-pr", Kind: workflow.GraphNodeDeterministic},
		},
		Edges: []workflow.GraphEdge{
			{Source: "implement", Target: "review"},
			{Source: "review", Target: "local-ci", Outcome: "pass"},
			{Source: "review", Target: "implement", Outcome: "needs-changes"},
			{Source: "local-ci", Target: "local-gate"},
			{Source: "local-gate", Target: "open-pr", Outcome: "pass"},
			{Source: "local-gate", Target: "implement", Outcome: "fail"},
			{Source: "open-pr", Terminal: workflow.GraphTerminalComplete},
		},
	}
}

func tev(seq uint64, t journal.EventType, mutate func(*journal.Event)) journal.Event {
	e := journal.Event{Schema: journal.EventSchema, Seq: seq, Type: t}
	if mutate != nil {
		mutate(&e)
	}
	return e
}

// TestProjectTransitionsUnavailableWithoutPinnedGraph is the issue's explicit
// "do not fabricate" requirement: a run with no pinned graph (an older run,
// or one whose snapshot could not be resolved) gets an explicit unavailable
// status and no rows, never a best-effort guess.
func TestProjectTransitionsUnavailableWithoutPinnedGraph(t *testing.T) {
	rows, status := ProjectTransitions([]journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
	}, nil)
	if status != TransitionsUnavailable {
		t.Fatalf("status = %q, want %q", status, TransitionsUnavailable)
	}
	if rows != nil {
		t.Fatalf("rows = %+v, want nil", rows)
	}
}

// TestProjectTransitionsFirstPassEmphasizesOnlyTheSelectedEdge is #1430's
// acceptance criterion 1 proven at the read-model layer: a first-pass run
// visits both implement and review — the endpoints of the declared
// needs-changes back-edge — but never crosses it, so it must not appear.
func TestProjectTransitionsFirstPassEmphasizesOnlyTheSelectedEdge(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(2, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(3, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }),
		tev(4, journal.EventGateEvaluated, func(e *journal.Event) { e.Gate, e.Target, e.Verdict = "review", "local-ci", "pass" }),
		tev(5, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "local-ci" }),
		tev(6, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "local-ci", "success" }),
		tev(7, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "local-gate" }),
		tev(8, journal.EventGateEvaluated, func(e *journal.Event) { e.Gate, e.Target, e.Verdict = "local-gate", "open-pr", "pass" }),
		tev(9, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "open-pr" }),
		tev(10, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "open-pr", "success" }),
		tev(11, journal.EventRunFinished, func(e *journal.Event) { e.Status = string(journal.PhaseCompleted) }),
	}

	rows, status := ProjectTransitions(events, implementationGraph())
	if status != TransitionsProjected {
		t.Fatalf("status = %q, want %q", status, TransitionsProjected)
	}
	want := []TransitionRow{
		{Ordinal: 0, Seq: 3, Source: "implement", Target: "review", Repass: false},
		{Ordinal: 1, Seq: 4, Source: "review", Target: "local-ci", Verdict: "pass", Repass: false},
		{Ordinal: 2, Seq: 7, Source: "local-ci", Target: "local-gate", Repass: false},
		{Ordinal: 3, Seq: 8, Source: "local-gate", Target: "open-pr", Verdict: "pass", Repass: false},
		{Ordinal: 4, Seq: 11, Source: "open-pr", Terminal: true, TerminalStatus: "completed"},
	}
	assertRows(t, rows, want)
	for _, row := range rows {
		if row.Source == "review" && row.Target == "implement" {
			t.Fatalf("rows = %+v, the untaken needs-changes->implement edge must not appear", rows)
		}
	}
}

func TestProjectTransitionsIncludesGateOverrideBranch(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventGateEvaluated, func(e *journal.Event) {
			e.Gate, e.Target, e.Verdict = "review", workflow.TargetEscalate, "needs-changes"
		}),
		tev(2, journal.EventRunFinished, func(e *journal.Event) { e.Status = string(journal.PhaseEscalated) }),
		tev(3, journal.EventGateOverridden, func(e *journal.Event) {
			e.Gate, e.Target, e.Verdict = "review", "implement", "needs-changes"
		}),
	}
	rows, status := ProjectTransitions(events, implementationGraph())
	if status != TransitionsProjected {
		t.Fatalf("status = %q, want %q", status, TransitionsProjected)
	}
	last := rows[len(rows)-1]
	if last.Seq != 3 || last.Source != "review" || last.Target != "implement" ||
		last.Verdict != "needs-changes" || !last.Repass {
		t.Fatalf("override transition = %+v", last)
	}
}

// TestProjectTransitionsRepassAtGateEvalSeq is #1430's acceptance criterion 2:
// a reviewer-repass run emphasizes the dotted needs-changes -> implement edge
// AT the causal gate-evaluation sequence.
func TestProjectTransitionsRepassAtGateEvalSeq(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(2, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(3, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }),
		tev(4, journal.EventGateEvaluated, func(e *journal.Event) { e.Gate, e.Target, e.Verdict = "review", "implement", "needs-changes" }),
		tev(5, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(6, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(7, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }),
		tev(8, journal.EventGateEvaluated, func(e *journal.Event) { e.Gate, e.Target, e.Verdict = "review", "local-ci", "pass" }),
	}
	rows, _ := ProjectTransitions(events, implementationGraph())
	repass := findRow(t, rows, 4)
	if repass.Source != "review" || repass.Target != "implement" || repass.Verdict != "needs-changes" || !repass.Repass {
		t.Fatalf("seq-4 row = %+v, want the repass edge review->implement", repass)
	}
	// The gate's own subsequent stage.started(implement) is arrival AT the
	// target the gate already named — not a second transition.
	for _, row := range rows {
		if row.Seq == 5 {
			t.Fatalf("rows = %+v, stage.started(implement) right after the gate named it must not produce its own row", rows)
		}
	}
	forward := findRow(t, rows, 8)
	if forward.Repass {
		t.Fatalf("seq-8 row = %+v, review->local-ci is a forward edge, not a repass", forward)
	}
}

// TestProjectTransitionsCyclicPrefixCompleteness is the exact defect the first
// prior attempt was rejected for: a repeatedly-traversed edge must produce one
// row PER occurrence, never deduplicated into a single row.
func TestProjectTransitionsCyclicPrefixCompleteness(t *testing.T) {
	var events []journal.Event
	seq := uint64(0)
	next := func() uint64 { seq++; return seq }
	events = append(events, tev(next(), journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }))
	events = append(events, tev(next(), journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }))
	const repasses = 3
	for i := 0; i < repasses; i++ {
		events = append(events, tev(next(), journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }))
		events = append(events, tev(next(), journal.EventGateEvaluated, func(e *journal.Event) {
			e.Gate, e.Target, e.Verdict = "review", "implement", "needs-changes"
		}))
		events = append(events, tev(next(), journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }))
		events = append(events, tev(next(), journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }))
	}
	events = append(events, tev(next(), journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }))
	events = append(events, tev(next(), journal.EventGateEvaluated, func(e *journal.Event) {
		e.Gate, e.Target, e.Verdict = "review", "local-ci", "pass"
	}))

	rows, _ := ProjectTransitions(events, implementationGraph())
	var repassRows []TransitionRow
	for _, row := range rows {
		if row.Source == "review" && row.Target == "implement" {
			repassRows = append(repassRows, row)
		}
	}
	if len(repassRows) != repasses {
		t.Fatalf("repass rows = %+v, want exactly %d (one per traversal, not deduplicated)", repassRows, repasses)
	}
	seen := map[uint64]bool{}
	for _, row := range repassRows {
		if seen[row.Seq] {
			t.Fatalf("repass rows = %+v, duplicate Seq %d", repassRows, row.Seq)
		}
		seen[row.Seq] = true
		if !row.Repass {
			t.Errorf("row %+v, want Repass=true", row)
		}
	}
}

// TestProjectTransitionsRetryDoesNotCreateTransition proves a stage retry (a
// second stage.started for the SAME name after a failed attempt) is never
// read as a graph edge.
func TestProjectTransitionsRetryDoesNotCreateTransition(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(2, journal.EventStageFinished, func(e *journal.Event) {
			e.Stage, e.Status, e.AttemptClass = "implement", "failure", journal.AttemptPolicy
		}),
		tev(3, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(4, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(5, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }),
	}
	rows, _ := ProjectTransitions(events, implementationGraph())
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly one row (implement->review); the retry must not appear", rows)
	}
	if rows[0].Source != "implement" || rows[0].Target != "review" || rows[0].Seq != 5 {
		t.Fatalf("row = %+v, want implement->review at seq 5", rows[0])
	}
}

// TestProjectTransitionsTaskSourcedFailureTerminal covers a task whose
// failure has no declared gate to route through (t.Next names a task or is
// empty) — the runner ends the run directly via finishStageFailure with no
// gate.evaluated in between. The transition still needs a row: source is the
// failed task, terminal, with the run's actual outcome status. No discrete
// target name exists for this case (only a gate's Target is journaled), so
// Target is deliberately left empty.
func TestProjectTransitionsTaskSourcedFailureTerminal(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(2, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "failure" }),
		tev(3, journal.EventRunFinished, func(e *journal.Event) { e.Status = string(journal.PhaseFailed) }),
	}
	rows, _ := ProjectTransitions(events, implementationGraph())
	assertRows(t, rows, []TransitionRow{
		{Ordinal: 0, Seq: 3, Source: "implement", Target: "", Terminal: true, TerminalStatus: "failed"},
	})
}

// TestProjectTransitionsGateSourcedReservedTargets covers a gate branching
// directly to a reserved control target — @abort and @escalate — proving the
// terminal status is derived correctly for each, and separately that a gate
// branching to "" (TerminalComplete) is recognized as terminal too, not
// mistaken for "no target set".
func TestProjectTransitionsGateSourcedReservedTargets(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantStatus string
	}{
		{"abort", workflow.TargetAbort, "aborted"},
		{"escalate", workflow.TargetEscalate, "escalated"},
		{"complete", workflow.TerminalComplete, "completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := []journal.Event{
				tev(1, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }),
				tev(2, journal.EventGateEvaluated, func(e *journal.Event) {
					e.Gate, e.Target, e.Verdict = "review", tc.target, "fail"
				}),
				tev(3, journal.EventRunFinished, func(e *journal.Event) { e.Status = tc.wantStatus }),
			}
			rows, _ := ProjectTransitions(events, implementationGraph())
			assertRows(t, rows, []TransitionRow{
				{Ordinal: 0, Seq: 2, Source: "review", Target: tc.target, Verdict: "fail", Terminal: true, TerminalStatus: tc.wantStatus},
			})
		})
	}
}

// TestProjectTransitionsParallelBranchesNoCrossBranchEdges is the acceptance
// criterion "parallel branches do not produce cross-branch phantom edges":
// two sibling branches each run their own two-stage sequence, and no row may
// connect one branch's stage to the other's.
func TestProjectTransitionsParallelBranchesNoCrossBranchEdges(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(2, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(3, journal.EventParallelStarted, func(e *journal.Event) { e.Parallel = "fanout" }),
		tev(4, journal.EventBranchStarted, func(e *journal.Event) { e.Branch, e.Parallel, e.BranchName = 1, "fanout", "a" }),
		tev(5, journal.EventStageStarted, func(e *journal.Event) { e.Branch, e.Stage = 1, "branch-a-1" }),
		tev(6, journal.EventStageFinished, func(e *journal.Event) { e.Branch, e.Stage, e.Status = 1, "branch-a-1", "success" }),
		tev(7, journal.EventStageStarted, func(e *journal.Event) { e.Branch, e.Stage = 1, "branch-a-2" }),
		tev(8, journal.EventStageFinished, func(e *journal.Event) { e.Branch, e.Stage, e.Status = 1, "branch-a-2", "success" }),
		tev(9, journal.EventBranchFinished, func(e *journal.Event) { e.Branch, e.Parallel, e.BranchStatus = 1, "fanout", journal.BranchSucceeded }),
		tev(10, journal.EventBranchStarted, func(e *journal.Event) { e.Branch, e.Parallel, e.BranchName = 2, "fanout", "b" }),
		tev(11, journal.EventStageStarted, func(e *journal.Event) { e.Branch, e.Stage = 2, "branch-b-1" }),
		tev(12, journal.EventStageFinished, func(e *journal.Event) { e.Branch, e.Stage, e.Status = 2, "branch-b-1", "success" }),
		tev(13, journal.EventBranchFinished, func(e *journal.Event) { e.Branch, e.Parallel, e.BranchStatus = 2, "fanout", journal.BranchSucceeded }),
		tev(14, journal.EventParallelFinished, func(e *journal.Event) { e.Parallel = "fanout" }),
		tev(15, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "join" }),
		tev(16, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "join", "success" }),
		tev(17, journal.EventRunFinished, func(e *journal.Event) { e.Status = string(journal.PhaseCompleted) }),
	}
	graph := implementationGraph()
	graph.Nodes = append(graph.Nodes,
		workflow.GraphNode{ID: "fanout", Kind: workflow.GraphNodeParallel},
		workflow.GraphNode{ID: "branch-a-1", Kind: workflow.GraphNodeDeterministic},
		workflow.GraphNode{ID: "branch-a-2", Kind: workflow.GraphNodeDeterministic},
		workflow.GraphNode{ID: "branch-b-1", Kind: workflow.GraphNodeDeterministic},
		workflow.GraphNode{ID: "join", Kind: workflow.GraphNodeDeterministic},
	)
	graph.Edges = append(graph.Edges,
		workflow.GraphEdge{Source: "implement", Target: "fanout"},
		workflow.GraphEdge{Source: "fanout", Target: "branch-a-1", Branch: "a"},
		workflow.GraphEdge{Source: "branch-a-1", Target: "branch-a-2"},
		workflow.GraphEdge{Source: "fanout", Target: "branch-b-1", Branch: "b"},
		workflow.GraphEdge{Source: "fanout", Target: "join"},
	)
	// implement already has an edge to "review" from the base fixture; this
	// test's events never touch review, so that edge is simply never taken —
	// it exercises the same "declared but untaken" discipline as the pass test.

	rows, status := ProjectTransitions(events, graph)
	if status != TransitionsProjected {
		t.Fatalf("status = %q, want %q", status, TransitionsProjected)
	}
	byBranch := map[int][]TransitionRow{}
	for _, row := range rows {
		byBranch[row.Branch] = append(byBranch[row.Branch], row)
	}
	assertRows(t, byBranch[1], []TransitionRow{
		{Branch: 1, Seq: 7, Source: "branch-a-1", Target: "branch-a-2"},
		{Branch: 1, Seq: 9, Source: "branch-a-2", Terminal: true, TerminalStatus: "succeeded"},
	})
	assertRows(t, byBranch[2], []TransitionRow{
		{Branch: 2, Seq: 13, Source: "branch-b-1", Terminal: true, TerminalStatus: "succeeded"},
	})
	for _, row := range rows {
		fromA := row.Source == "branch-a-1" || row.Source == "branch-a-2"
		toB := row.Target == "branch-b-1"
		fromB := row.Source == "branch-b-1"
		toA := row.Target == "branch-a-1" || row.Target == "branch-a-2"
		if (fromA && toB) || (fromB && toA) {
			t.Fatalf("rows = %+v, cross-branch phantom edge %+v", rows, row)
		}
	}
	// The root branch still connects through the parallel construct on
	// either side of it.
	root := byBranch[0]
	wantRoot := []TransitionRow{
		{Branch: 0, Seq: 3, Source: "implement", Target: "fanout"},
		{Branch: 0, Seq: 15, Source: "fanout", Target: "join"},
		{Branch: 0, Seq: 17, Source: "join", Terminal: true, TerminalStatus: "completed"},
	}
	assertRows(t, root, wantRoot)
}

// TestProjectTransitionsRerunIntoSettledParallelBranch is the exact defect the
// second prior attempt was rejected for: an operator stage.rerun.requested
// targets a stage inside a parallel branch whose branch.finished has ALREADY
// been journaled. The rerun's own events must form a fresh, independent
// occurrence — never fabricating an edge back to the branch's old, already-
// closed history, and the old occurrence's rows must survive unchanged.
func TestProjectTransitionsRerunIntoSettledParallelBranch(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventBranchStarted, func(e *journal.Event) { e.Branch, e.Parallel, e.BranchName = 1, "fanout", "a" }),
		tev(2, journal.EventStageStarted, func(e *journal.Event) { e.Branch, e.Stage = 1, "branch-a-1" }),
		tev(3, journal.EventStageFinished, func(e *journal.Event) { e.Branch, e.Stage, e.Status = 1, "branch-a-1", "success" }),
		tev(4, journal.EventBranchFinished, func(e *journal.Event) { e.Branch, e.Parallel, e.BranchStatus = 1, "fanout", journal.BranchSucceeded }),
		// ... later, an operator reruns branch-a-1 after the branch settled.
		tev(9, journal.EventStageRerunRequested, func(e *journal.Event) { e.Branch, e.Stage, e.Actor = 1, "branch-a-1", "operator" }),
		tev(10, journal.EventStageStarted, func(e *journal.Event) { e.Branch, e.Stage = 1, "branch-a-1" }),
		tev(11, journal.EventStageFinished, func(e *journal.Event) { e.Branch, e.Stage, e.Status = 1, "branch-a-1", "success" }),
		tev(12, journal.EventBranchFinished, func(e *journal.Event) { e.Branch, e.Parallel, e.BranchStatus = 1, "fanout", journal.BranchSucceeded }),
	}
	graph := implementationGraph()
	graph.Nodes = append(graph.Nodes, workflow.GraphNode{ID: "branch-a-1", Kind: workflow.GraphNodeDeterministic})

	rows, _ := ProjectTransitions(events, graph)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want exactly 2 (one terminal row per occurrence, no synthesized link between them)", rows)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	if rows[0].Occurrence == rows[1].Occurrence {
		t.Fatalf("rows = %+v, the rerun must start a NEW occurrence, not reuse the settled one", rows)
	}
	for _, row := range rows {
		if row.Source != "branch-a-1" || !row.Terminal || row.TerminalStatus != "succeeded" {
			t.Errorf("row = %+v, want a terminal branch-a-1 row for its own occurrence", row)
		}
		if row.Target != "" {
			t.Errorf("row = %+v, want no target — a rerun's own re-close is not a graph-declared edge", row)
		}
	}
}

// TestProjectTransitionsRerunAfterRunCompletionStartsFreshOccurrence mirrors
// the settled-parallel-branch case for the ROOT branch: a rerun after
// run.finished must not fabricate an edge from the run's old terminal state.
func TestProjectTransitionsRerunAfterRunCompletionStartsFreshOccurrence(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(2, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(3, journal.EventRunFinished, func(e *journal.Event) { e.Status = string(journal.PhaseCompleted) }),
		tev(4, journal.EventStageRerunRequested, func(e *journal.Event) { e.Stage, e.Actor = "implement", "operator" }),
		tev(5, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(6, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(7, journal.EventRunFinished, func(e *journal.Event) { e.Status = string(journal.PhaseCompleted) }),
	}
	rows, _ := ProjectTransitions(events, implementationGraph())
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want exactly 2 terminal rows (one per occurrence)", rows)
	}
	if rows[0].Seq != 3 || rows[1].Seq != 7 {
		t.Fatalf("rows = %+v, want terminal rows at seq 3 and 7 only", rows)
	}
	if rows[0].Occurrence == rows[1].Occurrence {
		t.Fatalf("rows = %+v, the post-completion rerun must be a new occurrence", rows)
	}
}

// TestProjectTransitionsIsDeterministic is §14.9's determinism property: the
// same event history always produces byte-identical rows.
func TestProjectTransitionsIsDeterministic(t *testing.T) {
	events := []journal.Event{
		tev(1, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(2, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(3, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }),
		tev(4, journal.EventGateEvaluated, func(e *journal.Event) { e.Gate, e.Target, e.Verdict = "review", "implement", "needs-changes" }),
		tev(5, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "implement" }),
		tev(6, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "implement", "success" }),
		tev(7, journal.EventGateStarted, func(e *journal.Event) { e.Gate = "review" }),
		tev(8, journal.EventGateEvaluated, func(e *journal.Event) { e.Gate, e.Target, e.Verdict = "review", "local-ci", "pass" }),
		tev(9, journal.EventRunFinished, func(e *journal.Event) { e.Status = string(journal.PhaseCompleted) }),
	}
	graph := implementationGraph()
	first, firstStatus := ProjectTransitions(events, graph)
	second, secondStatus := ProjectTransitions(events, graph)
	if firstStatus != secondStatus || !reflect.DeepEqual(first, second) {
		t.Fatalf("ProjectTransitions is not deterministic:\n first  = %+v (%s)\n second = %+v (%s)", first, firstStatus, second, secondStatus)
	}
}

func findRow(t *testing.T, rows []TransitionRow, seq uint64) TransitionRow {
	t.Helper()
	for _, row := range rows {
		if row.Seq == seq {
			return row
		}
	}
	t.Fatalf("rows = %+v, want a row at seq %d", rows, seq)
	return TransitionRow{}
}

// assertRows compares rows against want by (Branch, Seq, Source, Target,
// Verdict, Terminal, TerminalStatus, Repass) — ignoring Ordinal and
// Occurrence, which the caller sets to their zero value in most fixtures.
func assertRows(t *testing.T, rows []TransitionRow, want []TransitionRow) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v (%d), want %+v (%d)", rows, len(rows), want, len(want))
	}
	for i := range rows {
		got, exp := rows[i], want[i]
		got.Ordinal, exp.Ordinal = 0, 0
		got.Occurrence, exp.Occurrence = 0, 0
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("row[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
}
