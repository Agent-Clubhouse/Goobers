package workbenchgraph

import (
	"github.com/goobers/goobers/internal/workbench"
)

type nativeObservation struct {
	item     workbench.NodeRef
	relation workbench.NativeRelationship
}

func (p *projection) backlogs(windows []BacklogWindow) error {
	for _, window := range windows {
		source, ok := p.sources[window.SourceBindingID]
		if !ok || source.Spec.Kind != "backlog" {
			return ErrInvalidSnapshot
		}
		page := window.Page
		expected, err := workbench.SourceTargetDigest(p.scope, source)
		if err != nil || expected != page.SourceTargetDigest {
			return ErrInvalidSnapshot
		}
		c := p.coverage[window.SourceBindingID]
		if !validDigest(page.SourceTargetDigest) || len(page.Items) > workbench.MaxBacklogPageItems {
			return ErrInvalidSnapshot
		}
		if c.SourceTargetDigest != "" && c.SourceTargetDigest != page.SourceTargetDigest {
			return ErrInvalidSnapshot
		}
		c.SourceTargetDigest = page.SourceTargetDigest
		c.Status = "partial"
		addReason(c, "native-non-snapshot")
		for _, reason := range page.Reasons {
			addReason(c, reason)
		}
		// Native pagination lacks a source-wide revision pin. Even exhaustion cannot
		// prove missing records deleted, or make separately read pages coherent.
		if !page.Exhausted {
			addReason(c, "more-items")
		}
		if page.Omitted > 0 {
			addReason(c, "source-omissions")
		}
		for _, item := range page.Items {
			if item.Ref.SourceBindingID != window.SourceBindingID || item.Ref.Kind != "work-item" || p.scope.ValidateRef(item.Ref) != nil {
				return ErrInvalidSnapshot
			}
			if err := p.node(Observation{ContentDigest: digest(item), Ref: item.Ref, Title: item.Title, Type: item.Type, State: item.State, Revision: item.Revision, Locator: item.Locator, Objective: item.Objective}); err != nil {
				return err
			}
			if len(item.Relationships) > workbench.MaxSourceEdges {
				return ErrProjectionBound
			}
			for _, relation := range item.Relationships {
				if len(p.native) >= MaxEdges {
					return ErrProjectionBound
				}
				p.native = append(p.native, nativeObservation{item: item.Ref, relation: relation})
			}
			relationCoverage(c, item.RelationshipCoverage)
		}
	}
	return nil
}
func relationCoverage(c *Coverage, coverage workbench.RelationshipCoverage) {
	for _, part := range []struct{ field, value string }{{"parents", coverage.Parents}, {"blockers", coverage.Blockers}, {"milestones", coverage.Milestones}} {
		if part.value != "complete" {
			addReason(c, part.field+":"+part.value)
		}
	}
}
func (p *projection) nativeEdges() error {
	for _, observed := range p.native {
		edge, err := p.nativeEdge(observed)
		if err != nil {
			return err
		}
		if err = p.edge(edge); err != nil {
			return err
		}
	}
	return nil
}
func (p *projection) nativeEdge(observed nativeObservation) (Edge, error) {
	relation := observed.relation
	field := ""
	switch relation.Kind {
	case "parent-of":
		if !relation.Incoming {
			return Edge{}, ErrInvalidSnapshot
		}
		field = "parent"
	case "blocked-by":
		if relation.Incoming {
			return Edge{}, ErrInvalidSnapshot
		}
		field = "blockers"
	case "milestone-member":
		if relation.Incoming {
			return Edge{}, ErrInvalidSnapshot
		}
		field = "milestone"
	default:
		return Edge{}, ErrInvalidSnapshot
	}
	if (relation.Kind == "milestone-member") != (relation.Target.Kind == "milestone") {
		return Edge{}, ErrInvalidSnapshot
	}
	target, err := p.nativeEndpoint(observed.item, relation.Target)
	if err != nil {
		return Edge{}, err
	}
	current := observed.item
	edge := Edge{Kind: relation.Kind, From: Endpoint{Ref: &current}, To: target, Origin: "native", Owner: workbench.Owner{Kind: "native", SourceBindingID: current.SourceBindingID, Field: field, Object: current}}
	if relation.Incoming {
		edge.From, edge.To = edge.To, edge.From
	}
	return edge, nil
}
func (p *projection) nativeEndpoint(item workbench.NodeRef, target workbench.NativeTarget) (Endpoint, error) {
	if target.StableID == "" || (target.Kind != "work-item" && target.Kind != "milestone") {
		return Endpoint{}, ErrInvalidSnapshot
	}
	if target.Ref != nil {
		ref := *target.Ref
		if p.scope.ValidateRef(ref) != nil || ref.SourceBindingID != item.SourceBindingID || ref.SourceID != target.StableID || ref.Kind != target.Kind {
			return Endpoint{}, ErrInvalidSnapshot
		}
		return Endpoint{Ref: &ref}, nil
	}
	// An ADO link gains a scoped reference only when a separately authorized item
	// in this exact project binding proves target membership. Never guess from URL.
	expected := workbench.NodeRef{GaggleID: item.GaggleID, SourceBindingID: item.SourceBindingID, Kind: target.Kind, SourceID: target.StableID}
	if node := p.nodes[expected.Key()]; node != nil && !node.Conflict {
		for _, observed := range node.Observations {
			if observed.Ref == expected {
				return Endpoint{Ref: &expected}, nil
			}
		}
	}
	target.Ref = nil
	return Endpoint{Native: &target}, nil
}
