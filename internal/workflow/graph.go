package workflow

import "github.com/goobers/goobers/internal/workflow/internal/model"

// Graph is the deterministic projection of a compiled workflow machine.
type Graph = model.Graph

// GraphNodeKind identifies the execution kind of a graph node.
type GraphNodeKind = model.GraphNodeKind

// GraphNode is one task or gate in a workflow graph.
type GraphNode = model.GraphNode

// GraphTerminal identifies how an edge ends a run.
type GraphTerminal = model.GraphTerminal

// GraphEdge is one declared workflow transition.
type GraphEdge = model.GraphEdge

const (
	// GraphNodeDeterministic identifies a deterministic task.
	GraphNodeDeterministic = model.GraphNodeDeterministic
	// GraphNodeAgentic identifies an agentic task.
	GraphNodeAgentic = model.GraphNodeAgentic
	// GraphNodeGate identifies a gate.
	GraphNodeGate = model.GraphNodeGate

	// GraphTerminalComplete identifies successful completion.
	GraphTerminalComplete = model.GraphTerminalComplete
	// GraphTerminalAbort identifies blocked completion.
	GraphTerminalAbort = model.GraphTerminalAbort
	// GraphTerminalEscalate identifies escalated completion.
	GraphTerminalEscalate = model.GraphTerminalEscalate
)
