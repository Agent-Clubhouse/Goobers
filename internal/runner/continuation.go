package runner

import (
	"fmt"
	"sort"
	"strings"

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
	sourceKind, sourceOK := graphNodeKind(source, target)
	candidateKind, candidateOK := graphNodeKind(candidate, target)
	if !sourceOK || !candidateOK {
		return continuationTargetError(sourceDigest, candidateDigest, target, "target is not a resumable task or gate")
	}
	if sourceKind != candidateKind {
		return continuationTargetError(sourceDigest, candidateDigest, target,
			fmt.Sprintf("target kind changed from %q to %q", sourceKind, candidateKind))
	}
	if !sameOutgoingEdges(source, candidate, target) {
		return continuationTargetError(sourceDigest, candidateDigest, target, "target transition changed")
	}
	return nil
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

func graphNodeKind(machine *workflow.Machine, target string) (string, bool) {
	graph := machine.Graph()
	for _, node := range graph.Nodes {
		if node.ID != target {
			continue
		}
		switch node.Kind {
		case "deterministic", "agentic", "gate":
			return string(node.Kind), true
		default:
			return string(node.Kind), false
		}
	}
	return "", false
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
