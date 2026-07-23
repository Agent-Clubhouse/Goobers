package model

import (
	"sort"

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

// Graph node kinds distinguish task execution from gate evaluation.
const (
	GraphNodeDeterministic GraphNodeKind = "deterministic"
	GraphNodeAgentic       GraphNodeKind = "agentic"
	GraphNodeGate          GraphNodeKind = "gate"
)

// GraphNode is one task or gate in a workflow graph. Owner is the goober that
// executes an agentic task or evaluates an agentic gate.
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
// including the empty successful-completion target and reserved targets.
type GraphEdge struct {
	Source   string        `json:"source"`
	Target   string        `json:"target"`
	Outcome  string        `json:"outcome,omitempty"`
	Terminal GraphTerminal `json:"terminal,omitempty"`
}

// Graph returns the canonical graph projection of the compiled machine.
func (m *Machine) Graph() Graph {
	nodes := make([]GraphNode, 0, len(m.Def.Spec.Tasks)+len(m.Def.Spec.Gates))
	edges := make([]GraphEdge, 0, graphEdgeCount(m.Def.Spec))

	for _, task := range m.Def.Spec.Tasks {
		nodes = append(nodes, GraphNode{
			ID:    task.Name,
			Kind:  GraphNodeKind(task.Type),
			Owner: task.Goober,
		})
		edges = append(edges, GraphEdge{
			Source:   task.Name,
			Target:   task.Next,
			Terminal: graphTerminal(task.Next),
		})
	}

	for _, gate := range m.Def.Spec.Gates {
		node := GraphNode{
			ID:        gate.Name,
			Kind:      GraphNodeGate,
			Evaluator: gate.Evaluator,
		}
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil {
			node.Owner = gate.Agentic.Goober
		}
		nodes = append(nodes, node)

		for _, outcome := range graphOutcomes(gate.Branches) {
			target := gate.Branches[outcome]
			edges = append(edges, GraphEdge{
				Source:   gate.Name,
				Target:   target,
				Outcome:  outcome,
				Terminal: graphTerminal(target),
			})
		}
	}

	return Graph{
		Name:    m.Def.Name,
		Version: m.Def.Version,
		Digest:  m.Digest(),
		Start:   m.Def.Spec.Start,
		Nodes:   nodes,
		Edges:   edges,
	}
}

func graphEdgeCount(spec apiv1.WorkflowSpec) int {
	count := len(spec.Tasks)
	for _, gate := range spec.Gates {
		count += len(gate.Branches)
	}
	return count
}

func graphOutcomes(branches map[string]string) []string {
	outcomes := make([]string, 0, len(branches))
	for _, outcome := range []string{"pass", "fail"} {
		if _, ok := branches[outcome]; ok {
			outcomes = append(outcomes, outcome)
		}
	}

	remaining := make([]string, 0, len(branches)-len(outcomes))
	for outcome := range branches {
		if outcome != "pass" && outcome != "fail" {
			remaining = append(remaining, outcome)
		}
	}
	sort.Strings(remaining)
	return append(outcomes, remaining...)
}

func graphTerminal(target string) GraphTerminal {
	switch target {
	case TerminalComplete:
		return GraphTerminalComplete
	case TargetAbort:
		return GraphTerminalAbort
	case TargetEscalate:
		return GraphTerminalEscalate
	default:
		return ""
	}
}
