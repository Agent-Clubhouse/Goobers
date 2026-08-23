package runner

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

// ValidateContinuationTarget admits a continuation only at a resumable task or
// gate whose transition semantics are unchanged between the source and
// candidate machines.
func ValidateContinuationTarget(source, candidate *workflow.Machine, target string) error {
	target = strings.TrimSpace(target)
	sourceDigest := machineDigest(source)
	candidateDigest := machineDigest(candidate)
	if target == "" {
		return continuationTargetError(sourceDigest, candidateDigest, target, "target is required")
	}
	if source == nil || candidate == nil {
		return continuationTargetError(sourceDigest, candidateDigest, target, "workflow graph is unavailable")
	}
	if workflow.IsReservedAnyTarget(target) || target == workflow.TerminalComplete {
		return continuationTargetError(sourceDigest, candidateDigest, target, "target is reserved or terminal")
	}
	sourceNode, sourceOK := graphNode(source, target)
	candidateNode, candidateOK := graphNode(candidate, target)
	if !sourceOK || !candidateOK {
		return continuationTargetError(sourceDigest, candidateDigest, target, "target is not a resumable task or gate")
	}
	if sourceNode.Kind != candidateNode.Kind {
		return continuationTargetError(sourceDigest, candidateDigest, target,
			fmt.Sprintf("target kind changed from %q to %q", sourceNode.Kind, candidateNode.Kind))
	}
	if !sameExecutionSemantics(source, candidate, target, sourceNode.Kind) {
		return continuationTargetError(sourceDigest, candidateDigest, target, "target execution semantics changed")
	}
	if !sameOutgoingEdges(source, candidate, target) {
		return continuationTargetError(sourceDigest, candidateDigest, target, "target transition changed")
	}
	return nil
}

type continuationExecutionProjection struct {
	Kind workflow.GraphNodeKind
	Task *apiv1.Task
	Gate *apiv1.Gate
}

func sameExecutionSemantics(source, candidate *workflow.Machine, target string, kind workflow.GraphNodeKind) bool {
	return reflect.DeepEqual(executionProjection(source, target, kind), executionProjection(candidate, target, kind))
}

func executionProjection(machine *workflow.Machine, target string, kind workflow.GraphNodeKind) continuationExecutionProjection {
	projection := continuationExecutionProjection{Kind: kind}
	switch kind {
	case workflow.GraphNodeDeterministic, workflow.GraphNodeAgentic:
		task, ok := machine.Task(target)
		if ok {
			task.Name = ""
			projection.Task = &task
		}
	case workflow.GraphNodeGate:
		gate, ok := machine.Gate(target)
		if ok {
			gate.Name = ""
			projection.Gate = &gate
		}
	}
	return projection
}

func machineDigest(machine *workflow.Machine) string {
	if machine == nil {
		return "<nil>"
	}
	return machine.Digest()
}

func continuationTargetError(sourceDigest, candidateDigest, target, reason string) error {
	return fmt.Errorf("continuation target %q rejected: %s (source digest %q, candidate digest %q)",
		target, reason, sourceDigest, candidateDigest)
}

func graphNode(machine *workflow.Machine, target string) (workflow.GraphNode, bool) {
	graph := machine.Graph()
	for _, node := range graph.Nodes {
		if node.ID != target {
			continue
		}
		switch node.Kind {
		case workflow.GraphNodeDeterministic, workflow.GraphNodeAgentic, workflow.GraphNodeGate:
			return node, true
		default:
			return workflow.GraphNode{}, false
		}
	}
	return workflow.GraphNode{}, false
}

func sameOutgoingEdges(source, candidate *workflow.Machine, target string) bool {
	sourceEdges := outgoingEdgeKeys(source, target)
	candidateEdges := outgoingEdgeKeys(candidate, target)
	if len(sourceEdges) != len(candidateEdges) {
		return false
	}
	for i := range sourceEdges {
		if sourceEdges[i] != candidateEdges[i] {
			return false
		}
	}
	return true
}

func outgoingEdgeKeys(machine *workflow.Machine, target string) []string {
	var edges []string
	for _, edge := range machine.Graph().Edges {
		if edge.Source != target {
			continue
		}
		edges = append(edges, fmt.Sprintf("%s\x00%s\x00%s\x00%s",
			edge.Target, edge.Outcome, edge.Terminal, edge.Branch))
	}
	sort.Strings(edges)
	return edges
}
