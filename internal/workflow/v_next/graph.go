package vnext

import (
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

func buildGraph(def Definition) model.Graph {
	nodes := make([]model.GraphNode, 0, len(def.Spec.Tasks)+len(def.Spec.Gates)+len(def.Spec.Parallels))
	edges := make([]model.GraphEdge, 0, graphEdgeCount(def.Spec))
	joinTargets := graphJoinTargets(def)

	for _, task := range def.Spec.Tasks {
		target := graphTarget(task.Name, task.Next, joinTargets)
		nodes = append(nodes, model.GraphNode{
			ID:    task.Name,
			Kind:  model.GraphNodeKind(task.Type),
			Owner: task.Goober,
		})
		edges = append(edges, model.GraphEdge{
			Source:   task.Name,
			Target:   target,
			Terminal: graphTerminal(target),
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
			target := graphTarget(gate.Name, gate.Branches[outcome], joinTargets)
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
		// Fan-out edges stay in declaration order, the same order that assigns
		// branch ids, independent of how the runner schedules the branches.
		for _, branch := range parallel.Branches {
			edges = append(edges, model.GraphEdge{
				Source: parallel.Name,
				Target: branch.Start,
				Branch: branch.Name,
			})
		}
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
		count += len(parallel.Branches)
		if parallel.OnFailure != "" {
			count++
		}
	}
	return count
}

type graphStateIndex struct {
	states map[string][]string
}

func newGraphStateIndex(def Definition) graphStateIndex {
	states := make(map[string][]string, len(def.Spec.Tasks)+len(def.Spec.Gates)+len(def.Spec.Parallels))
	for _, task := range def.Spec.Tasks {
		states[task.Name] = []string{task.Next}
	}
	for _, gate := range def.Spec.Gates {
		for _, outcome := range graphOutcomes(gate.Branches) {
			states[gate.Name] = append(states[gate.Name], gate.Branches[outcome])
		}
	}
	for _, parallel := range def.Spec.Parallels {
		for _, branch := range parallel.Branches {
			states[parallel.Name] = append(states[parallel.Name], branch.Start)
		}
		states[parallel.Name] = append(states[parallel.Name], parallel.Join)
		if parallel.OnFailure != "" {
			states[parallel.Name] = append(states[parallel.Name], parallel.OnFailure)
		}
	}
	return graphStateIndex{states: states}
}

func (g graphStateIndex) Has(state string) bool {
	_, ok := g.states[state]
	return ok
}

func (g graphStateIndex) Outgoing(state string) []string {
	return g.states[state]
}

func graphJoinTargets(def Definition) map[string]string {
	index := newGraphStateIndex(def)
	targets := make(map[string]string)
	for _, parallel := range def.Spec.Parallels {
		for _, branch := range parallel.Branches {
			for _, terminal := range joinTerminalStates(index, branch.Start) {
				targets[terminal] = parallel.Join
			}
		}
	}
	return targets
}

func graphTarget(source, target string, joinTargets map[string]string) string {
	if target == TargetJoin {
		if join, ok := joinTargets[source]; ok {
			return join
		}
	}
	return target
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
