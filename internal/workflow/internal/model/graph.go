package model

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Graph is the canonical, deterministic projection of a compiled workflow
// machine. Its slices are ordered and contain no maps, so JSON serialization is
// stable for identical compiled definitions.
type Graph struct {
	Name    string      `json:"name"`
	Version int         `json:"version"`
	Digest  string      `json:"digest"`
	Start   string      `json:"start"`
	Nodes   []GraphNode `json:"nodes"`
	Edges   []GraphEdge `json:"edges"`
}

// GraphNodeKind identifies the visual and execution kind of a graph node.
type GraphNodeKind string

// Graph node kinds distinguish task execution from gate evaluation and
// parallel fan-out.
const (
	GraphNodeDeterministic GraphNodeKind = "deterministic"
	GraphNodeAgentic       GraphNodeKind = "agentic"
	GraphNodeGate          GraphNodeKind = "gate"
	GraphNodeParallel      GraphNodeKind = "parallel"
)

// GraphNode is one task, gate, or parallel in a workflow graph. Owner is the
// goober that executes an agentic task or evaluates an agentic gate.
type GraphNode struct {
	ID        string              `json:"id"`
	Kind      GraphNodeKind       `json:"kind"`
	Owner     string              `json:"owner,omitempty"`
	Evaluator apiv1.EvaluatorKind `json:"evaluator,omitempty"`
}

// GraphTerminal identifies how an edge ends a run.
type GraphTerminal string

// Graph terminal outcomes distinguish each way an edge can end a run.
const (
	GraphTerminalComplete GraphTerminal = "complete"
	GraphTerminalAbort    GraphTerminal = "abort"
	GraphTerminalEscalate GraphTerminal = "escalate"
)

// GraphEdge is one declared transition. Target retains the machine target,
// including the empty successful-completion target and terminal reserved
// targets. Parallel branch "@join" targets are resolved to the concrete join
// node so converging edges describe the executable graph.
//
// Branch names the parallel branch on fan-out edges leaving a parallel state.
// It is empty on ordinary sequential and converging join edges.
type GraphEdge struct {
	Source   string        `json:"source"`
	Target   string        `json:"target"`
	Outcome  string        `json:"outcome,omitempty"`
	Terminal GraphTerminal `json:"terminal,omitempty"`
	Branch   string        `json:"branch,omitempty"`
}

// Graph returns the interpreter-built canonical graph for the compiled machine.
func (m *Machine) Graph() Graph {
	return cloneGraph(m.graph)
}

func cloneGraph(graph Graph) Graph {
	if graph.Nodes != nil {
		graph.Nodes = append([]GraphNode{}, graph.Nodes...)
	}
	if graph.Edges != nil {
		graph.Edges = append([]GraphEdge{}, graph.Edges...)
	}
	return graph
}
