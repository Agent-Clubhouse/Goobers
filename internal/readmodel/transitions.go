package readmodel

import (
	"sort"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// TransitionRow is one EXECUTED workflow-graph transition (#1427) — the
// portal's run graph (#1430) must render a repass edge as taken only when the
// run's journal actually crossed it, not merely because both endpoint nodes
// were visited (a cyclic workflow can visit both ends of an untaken repass
// edge for unrelated reasons).
//
// Ordinal is the row's stable position in the run's full ordered transition
// history (0-based); Seq is the causal journal sequence number of the event
// that established the transition — a gate.evaluated for a conditional
// branch, or the arrival/closing event for an unconditional task edge. Seq
// alone is already a unique, deterministic sort key within a run (§3.3: Seq
// is a per-run monotonic counter from 1), so Ordinal is redundant with it but
// kept as an explicit, storage-friendly row identity.
type TransitionRow struct {
	RunID string

	Ordinal    int
	Branch     int
	Occurrence int
	Seq        uint64

	// Source is the stage or gate name the run was at when this transition
	// fired.
	Source string
	// Target is the node this transition arrived at. Empty when Terminal is
	// true AND the source was a task — a task's own declared Next (unlike a
	// gate's) is never journaled as a discrete field, so the terminal
	// destination is known only via TerminalStatus, not a named target. When
	// the source was a GATE, Target carries the gate's own reserved constant
	// ("", "@abort", "@escalate") verbatim, since gate.evaluated always
	// journals it.
	Target string
	// Verdict is the gate's decision when Source is a gate; empty otherwise.
	Verdict string

	Terminal bool
	// TerminalStatus is the run's or branch's actual outcome status
	// (journal.RunPhase for branch 0, journal.BranchStatus for a declared
	// branch) when Terminal is true.
	TerminalStatus string

	// Repass reports whether the declared edge this transition took is a
	// back-edge in the run's pinned workflow graph (BFS discovery depth of
	// Target <= depth of Source from the graph's start, mirroring the
	// portal's own classification — portal/src/workflowGraph.ts). Always
	// false when Terminal, matching the portal's own rule that a terminal
	// edge is never a repass.
	Repass bool
}

// TransitionsUnavailable is ProjectTransitions' status when a run has no
// pinned workflow-graph snapshot to classify edges against — an older run
// predating #1427's dependency (#1917's journal.PinnedWorkflowGraphInputName)
// or one whose snapshot could not be read. Never fabricate transitions
// without the graph: repass classification and terminal/target validation
// both need it.
const TransitionsUnavailable = "unavailable"

// TransitionsProjected is ProjectTransitions' status for a run whose pinned
// graph was available, whether or not the run produced any events yet.
const TransitionsProjected = "projected"

// ProjectTransitions folds a run's full event history into its ordered,
// exact executed-transition history. graph is the run's PINNED workflow graph
// (immutable per run, read from the journal.PinnedWorkflowGraphInputName
// input snapshot) — not the current/live compiled workflow, which can differ
// from what this run actually ran. A nil graph returns (nil,
// TransitionsUnavailable) rather than guessing.
//
// Like ProjectRun, this is a pure function of its inputs — no clock, no
// filesystem beyond what the caller already resolved into graph and events —
// so replaying a run's complete event history always reproduces the same
// ordered transitions (§14.9's determinism requirement, and the
// incremental-equals-whole property project_test.go's pattern proves for the
// run projector).
//
// # Why a flat per-event walk, not a graph-shaped state machine
//
// Two earlier attempts at this projection were rejected in review for
// exactly the two failure modes this design is built to avoid:
//
//   - "Cyclic prefix completeness": a run that traverses the same declared
//     edge more than once (an ordinary repass loop) must produce ONE row per
//     occurrence, not a deduplicated set. This function never dedupes by
//     (source, target) — it appends a row every time the walk observes the
//     journal cross an edge, so a five-times-repassed gate produces five
//     rows.
//   - "Reruns into previously settled parallel branches": an operator's
//     stage.rerun.requested can re-enter a branch whose parallel.finished (or
//     branch.finished) has already been journaled. This function splits each
//     branch id's events into "occurrences" — maximal runs bounded by a
//     fresh-start signal (the branch's first event, or a rerun request
//     naming a stage in it) and a close signal (branch.finished, or
//     run.finished for the root branch 0). A rerun's own events start a NEW
//     occurrence with no synthesized incoming edge from the branch's earlier,
//     already-closed history — so the old and new occurrences never bleed
//     into each other.
func ProjectTransitions(events []journal.Event, graph *workflow.Graph) ([]TransitionRow, string) {
	if graph == nil {
		return nil, TransitionsUnavailable
	}
	depth := graphDepths(graph)

	byBranch := map[int][]journal.Event{}
	var branches []int
	for _, event := range events {
		if !event.KnownSchema() {
			continue
		}
		if _, ok := byBranch[event.Branch]; !ok {
			branches = append(branches, event.Branch)
		}
		byBranch[event.Branch] = append(byBranch[event.Branch], event)
	}
	sort.Ints(branches)

	var rows []TransitionRow
	for _, branch := range branches {
		rows = append(rows, branchTransitions(byBranch[branch], branch, depth)...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	for i := range rows {
		rows[i].Ordinal = i
	}
	return rows, TransitionsProjected
}

// branchTransitions splits one branch id's events into occurrences and walks
// each independently. A branch that never reruns after settling has exactly
// one occurrence; a branch rerun after its parallel.finished/branch.finished
// produces a second, unrelated to the first.
func branchTransitions(events []journal.Event, branch int, depth map[string]int) []TransitionRow {
	var rows []TransitionRow
	occurrence := 0
	var current []journal.Event
	flush := func() {
		if len(current) == 0 {
			return
		}
		rows = append(rows, walkOccurrence(current, branch, occurrence, depth)...)
		occurrence++
		current = nil
	}
	for _, event := range events {
		switch event.Type {
		case journal.EventStageRerunRequested:
			// A rerun can target any historical stage, not necessarily the
			// branch's current position — it is an operator override, never a
			// traversed edge. Close whatever was open (a no-op if the branch
			// had already settled) and start fresh at the rerun's own target,
			// so its transitions never inherit an edge from the branch's
			// prior, possibly-already-closed history.
			flush()
			current = append(current, event)
		case journal.EventRunFinished, journal.EventBranchFinished:
			current = append(current, event)
			flush()
		default:
			current = append(current, event)
		}
	}
	flush()
	return rows
}

// walkOccurrence is the flat per-event replay for one occurrence of one
// branch: position tracks the node the branch is currently "at", and a row is
// appended every time an event moves position to a DIFFERENT node (a retry —
// another stage.started/gate.started for the SAME name — never moves
// position, so it never produces a row). position starts empty, so the first
// arrival in a fresh occurrence never gets a synthesized incoming edge.
func walkOccurrence(events []journal.Event, branch, occurrence int, depth map[string]int) []TransitionRow {
	var rows []TransitionRow
	position := ""

	arrive := func(name string, seq uint64) {
		if position != "" && position != name {
			rows = append(rows, TransitionRow{
				Branch: branch, Occurrence: occurrence, Seq: seq,
				Source: position, Target: name, Repass: isRepass(depth, position, name),
			})
		}
		position = name
	}
	closeTerminal := func(seq uint64, status string) {
		if position != "" {
			rows = append(rows, TransitionRow{
				Branch: branch, Occurrence: occurrence, Seq: seq,
				Source: position, Terminal: true, TerminalStatus: status,
			})
		}
		position = ""
	}

	for _, event := range events {
		switch event.Type {
		case journal.EventStageRerunRequested:
			position = event.Stage
		case journal.EventRunResumed:
			// A resume continues at the same position the crash interrupted —
			// never a traversed edge.
			position = event.Target
		case journal.EventStageStarted:
			arrive(event.Stage, event.Seq)
		case journal.EventGateStarted:
			arrive(event.Gate, event.Seq)
		case journal.EventParallelStarted:
			// The parallel construct is itself a graph node (GraphNodeParallel).
			// Its own outgoing edge (to Join or OnFailure) is never journaled
			// discretely — unlike a gate, a parallel's completion carries no
			// Target field — but it needs none: whatever real event follows
			// parallel.finished on this (root) branch's timeline arrives via
			// the ordinary arrive()/closeTerminal() path below, using
			// whichever node the runtime actually dispatched next. Position is
			// deliberately left at the parallel's name across
			// parallel.finished (no case handles that event type here) so the
			// eventual arrival records the correct source.
			arrive(event.Parallel, event.Seq)
		case journal.EventGateEvaluated, journal.EventGateOverridden:
			source := position
			if source == "" {
				source = event.Gate
			}
			if workflow.IsReservedTarget(event.Target) ||
				event.Target == workflow.TerminalComplete ||
				event.Target == journal.TargetComplete {
				rows = append(rows, TransitionRow{
					Branch: branch, Occurrence: occurrence, Seq: event.Seq,
					Source: source, Target: event.Target, Verdict: event.Verdict,
					Terminal: true, TerminalStatus: reservedTargetStatus(event.Target),
				})
				position = ""
			} else {
				rows = append(rows, TransitionRow{
					Branch: branch, Occurrence: occurrence, Seq: event.Seq,
					Source: source, Target: event.Target, Verdict: event.Verdict,
					Repass: isRepass(depth, source, event.Target),
				})
				position = event.Target
			}
		case journal.EventRunFinished:
			closeTerminal(event.Seq, event.Status)
		case journal.EventBranchFinished:
			closeTerminal(event.Seq, string(event.BranchStatus))
		}
	}
	return rows
}

// reservedTargetStatus maps a gate's reserved terminal target to the run
// phase it produces, so a gate-sourced terminal transition's TerminalStatus
// is known immediately at the gate.evaluated event rather than waiting for
// the run.finished that follows it (which the walk would otherwise also try
// to close against, double-counting — closeTerminal's position=="" guard
// after this branch is what prevents that).
func reservedTargetStatus(target string) string {
	switch target {
	case workflow.TargetAbort:
		return string(journal.PhaseAborted)
	case workflow.TargetEscalate:
		return string(journal.PhaseEscalated)
	default: // workflow.TerminalComplete or journal.TargetComplete
		return string(journal.PhaseCompleted)
	}
}

// isRepass reports whether source->target is a back-edge, using the same
// discovery-depth rule the portal renders with (portal/src/workflowGraph.ts:
// repass = !edge.terminal && depth[target] <= depth[source]). Both names must
// have a known depth — an edge naming a node absent from the pinned graph
// (should not happen for a graph-conformant run, but never assumed) is never
// classified as a repass.
func isRepass(depth map[string]int, source, target string) bool {
	sourceDepth, sourceOK := depth[source]
	targetDepth, targetOK := depth[target]
	return sourceOK && targetOK && targetDepth <= sourceDepth
}

// graphDepths computes each node's BFS discovery depth from graph.Start,
// following only non-terminal edges — the exact algorithm
// portal/src/workflowGraph.ts uses to classify a repass edge, ported to Go so
// the read model and the portal can never disagree about which edges are
// back-edges. A node unreachable from Start (should not occur in a
// compiler-admitted graph, but not assumed) is visited from its own fresh
// root one level past the deepest depth seen so far, exactly mirroring the
// TypeScript fallback.
func graphDepths(graph *workflow.Graph) map[string]int {
	outgoing := map[string][]workflow.GraphEdge{}
	nodeByID := map[string]bool{}
	for _, node := range graph.Nodes {
		nodeByID[node.ID] = true
	}
	for _, edge := range graph.Edges {
		outgoing[edge.Source] = append(outgoing[edge.Source], edge)
	}

	depth := map[string]int{}
	visitFrom := func(root string, rootDepth int) {
		if _, seen := depth[root]; seen {
			return
		}
		depth[root] = rootDepth
		queue := []string{root}
		for i := 0; i < len(queue); i++ {
			source := queue[i]
			for _, edge := range outgoing[source] {
				if edge.Terminal != "" || !nodeByID[edge.Target] {
					continue
				}
				if _, seen := depth[edge.Target]; seen {
					continue
				}
				depth[edge.Target] = depth[source] + 1
				queue = append(queue, edge.Target)
			}
		}
	}

	if nodeByID[graph.Start] {
		visitFrom(graph.Start, 0)
	}
	for _, node := range graph.Nodes {
		if _, seen := depth[node.ID]; !seen {
			visitFrom(node.ID, maxDepth(depth)+1)
		}
	}
	return depth
}

func maxDepth(depth map[string]int) int {
	max := -1
	for _, d := range depth {
		if d > max {
			max = d
		}
	}
	return max
}
