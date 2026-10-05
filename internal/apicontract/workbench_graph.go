package apicontract

import (
	"strings"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchgraph"
)

// WorkbenchGraphPath reads an authorized server-selected source graph.
const WorkbenchGraphPath = V1Prefix + "/gaggles/{gaggle}/workbench/graph"

// RouteWorkbenchGraph identifies the bounded read-only graph route.
const RouteWorkbenchGraph RouteID = "workbenchGraph"

// WorkbenchGraph is a rebuildable source observation, never planning truth.
type WorkbenchGraph = workbenchgraph.Graph

func workbenchGraphFixture() WorkbenchGraph {
	ref := workbench.NodeRef{GaggleID: "web", SourceBindingID: "backlog", Kind: "work-item", SourceID: "987654"}
	target := workbench.NodeRef{GaggleID: "web", SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-00000000-0000-0000-0000-000000000001"}
	pin := &workbench.SourceProvenance{Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}
	return WorkbenchGraph{Generation: strings.Repeat("a", 64), GaggleID: "web", Partial: true,
		Nodes:     []workbenchgraph.Node{{Key: ref.Key(), Observations: []workbenchgraph.Observation{{ContentDigest: strings.Repeat("d", 64), Ref: ref, Title: "Explicit native objective", Type: "Issue", State: "open", Revision: "2026-10-04T12:00:00Z", Locator: workbench.SourceLocator{ID: "42"}, Objective: true}}}},
		Edges:     []workbenchgraph.Edge{{Key: strings.Repeat("b", 64), EdgeID: "edge-00000000-0000-0000-0000-000000000001", Kind: "contributes-to", From: workbenchgraph.Endpoint{Ref: &ref, Resolved: true}, To: workbenchgraph.Endpoint{Ref: &target}, Origin: "authored", Owner: workbenchgraph.Owner{Kind: "manifest", SourceBindingID: "links", Path: "links.yaml"}}},
		Documents: []workbenchgraph.Document{{SourceBindingID: "strategy", Path: "objective.md", Status: "available", Provenance: pin, Ref: &target}},
		Aliases:   []workbenchgraph.Alias{{Name: "delivery", Target: workbenchgraph.Endpoint{Ref: &target}, Owner: workbenchgraph.Owner{Kind: "manifest", SourceBindingID: "links", Path: "links.yaml"}}},
		Sources:   []workbenchgraph.Coverage{{SourceBindingID: "backlog", Kind: "backlog", Status: "partial", Consistency: "native-non-snapshot", SourceTargetDigest: strings.Repeat("e", 64), Reasons: []string{"native-non-snapshot"}}}, Conflicts: []workbenchgraph.Conflict{},
	}
}
