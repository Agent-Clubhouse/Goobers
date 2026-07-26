package vnext

import (
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

func buildGraph(def Definition) model.Graph {
	nodes := make([]model.GraphNode, 0, len(def.Spec.Tasks)+len(def.Spec.Gates)+len(def.Spec.Parallels))
	edges := make([]model.GraphEdge, 0, graphEdgeCount(def.Spec))

	for _, task := range def.Spec.Tasks {
		nodes = append(nodes, model.GraphNode{
			ID:    task.Name,
			Kind:  model.GraphNodeKind(task.Type),
			Owner: task.Goober,
		})
		edges = append(edges, model.GraphEdge{
			Source:   task.Name,
			Target:   task.Next,
			Terminal: graphTerminal(task.Next),
		})
	}

	for _, gate := range def.Spec.Gates {
		node := model.GraphNode{
			ID:        gate.Name,
			Kind:      model.GraphNodeGate,
			Evaluator: gate.Evaluator,
		}
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil {
			node.Owner = gate.Agentic.Goober
		}
		nodes = append(nodes, node)

		for _, outcome := range graphOutcomes(gate.Branches) {
			target := gate.Branches[outcome]
			edges = append(edges, model.GraphEdge{
				Source:   gate.Name,
				Target:   target,
				Outcome:  outcome,
				Terminal: graphTerminal(target),
			})
		}
	}

	for _, parallel := range def.Spec.Parallels {
		nodes = append(nodes, model.GraphNode{
			ID:   parallel.Name,
			Kind: model.GraphNodeParallel,
		})
		// Fan-out edges in declaration order — the same order that assigns
		// branch ids — then the join, then the failure route. Deterministic and
		// independent of how the runner schedules the branches.
		for _, branch := range parallel.Branches {
			edges = append(edges, model.GraphEdge{
				Source: parallel.Name,
				Target: branch.Start,
				Branch: branch.Name,
			})
		}
		edges = append(edges, model.GraphEdge{
			Source:   parallel.Name,
			Target:   parallel.Join,
			Outcome:  "join",
			Terminal: graphTerminal(parallel.Join),
		})
		if parallel.OnFailure != "" {
			edges = append(edges, model.GraphEdge{
				Source:   parallel.Name,
				Target:   parallel.OnFailure,
				Outcome:  "branch-failed",
				Terminal: graphTerminal(parallel.OnFailure),
			})
		}
	}

	return model.Graph{
		Start: def.Spec.Start,
		Nodes: nodes,
		Edges: edges,
	}
}

func graphEdgeCount(spec apiv1.WorkflowSpec) int {
	count := len(spec.Tasks)
	for _, gate := range spec.Gates {
		count += len(gate.Branches)
	}
	for _, parallel := range spec.Parallels {
		// one fan-out edge per branch, plus the join, plus onFailure when set
		count += len(parallel.Branches) + 1
		if parallel.OnFailure != "" {
			count++
		}
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

func graphTerminal(target string) model.GraphTerminal {
	switch target {
	case TerminalComplete:
		return model.GraphTerminalComplete
	case TargetAbort:
		return model.GraphTerminalAbort
	case TargetEscalate:
		return model.GraphTerminalEscalate
	default:
		return ""
	}
}
