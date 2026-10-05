package workbenchgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/goobers/goobers/internal/workbench"
)

type projection struct {
	scope      workbench.Scope
	graph      Graph
	sources    map[string]workbench.BoundSource
	coverage   map[string]*Coverage
	nodes      map[string]*Node
	edges      map[string]*Edge
	edgeIDs    map[string][]string
	edgeTuples map[string][]string
	documents  map[string]Document
	native     []nativeObservation
	manifest   *workbench.Owner
}

// Project rebuilds a bounded graph from the supplied authorized observations.
// It refuses mixed document commits/targets and invalid source scope. Source
// conflicts are returned visibly in Graph; they never resolve by scan order.
func Project(input Snapshot) (Graph, error) {
	if err := snapshotBounds(input); err != nil {
		return Graph{}, err
	}
	p, err := newProjection(input.Sources)
	if err != nil {
		return Graph{}, err
	}
	if err = p.backlogs(input.Backlogs); err != nil {
		return Graph{}, err
	}
	if err = p.documentPages(input.Documents); err != nil {
		return Graph{}, err
	}
	if err = p.nativeEdges(); err != nil {
		return Graph{}, err
	}
	p.finish()
	return p.graph, nil
}

func newProjection(set workbench.SourceSet) (*projection, error) {
	if set.Scope.Validate() != nil || len(set.Sources) != len(set.Scope.Bindings) {
		return nil, ErrInvalidSnapshot
	}
	p := &projection{scope: set.Scope, sources: map[string]workbench.BoundSource{}, coverage: map[string]*Coverage{}, nodes: map[string]*Node{}, edges: map[string]*Edge{}, edgeIDs: map[string][]string{}, edgeTuples: map[string][]string{}, documents: map[string]Document{}, manifest: set.ManifestOwner}
	p.graph = Graph{GaggleID: set.Scope.GaggleID, Nodes: []Node{}, Edges: []Edge{}, Documents: []Document{}, Aliases: []Alias{}, Sources: []Coverage{}, Conflicts: []Conflict{}}
	for _, s := range set.Sources {
		if !set.Scope.Bindings[s.Spec.Name] || p.sources[s.Spec.Name].Spec.Name != "" {
			return nil, ErrInvalidSnapshot
		}
		consistency := "commit-pinned"
		switch s.Spec.Kind {
		case "backlog":
			consistency = "native-non-snapshot"
		case "documents", "relationships":
		default:
			return nil, ErrInvalidSnapshot
		}
		p.sources[s.Spec.Name] = s
		p.coverage[s.Spec.Name] = &Coverage{SourceBindingID: s.Spec.Name, Kind: s.Spec.Kind, Status: "not-read", Consistency: consistency}
	}
	if p.manifest != nil {
		s := p.sources[p.manifest.SourceBindingID]
		if p.manifest.Kind != "manifest" || s.Spec.Kind != "relationships" || len(s.Spec.Paths) != 1 || s.Spec.Paths[0] != p.manifest.Path || p.manifest.Object != (workbench.NodeRef{}) || p.manifest.Field != "" {
			return nil, ErrInvalidSnapshot
		}
	}
	return p, nil
}

func (p *projection) node(value Observation) error {
	if p.scope.ValidateRef(value.Ref) != nil || value.Ref.SourceBindingID == "" {
		return ErrInvalidSnapshot
	}
	key := value.Ref.Key()
	n := p.nodes[key]
	if n == nil {
		if len(p.nodes) >= MaxNodes {
			return ErrProjectionBound
		}
		n = &Node{Key: key}
		p.nodes[key] = n
	}
	for _, old := range n.Observations {
		if reflect.DeepEqual(old, value) {
			return nil
		}
	}
	n.Observations = append(n.Observations, value)
	if len(n.Observations) > 1 {
		n.Conflict = true
		p.coverage[value.Ref.SourceBindingID].Status = "partial"
	}
	return nil
}

func (p *projection) edge(value Edge) error {
	value.Key = digest(value)
	if p.edges[value.Key] != nil {
		return nil
	}
	if len(p.edges) >= MaxEdges {
		return ErrProjectionBound
	}
	p.edges[value.Key] = &value
	if value.EdgeID != "" {
		p.edgeIDs[value.EdgeID] = append(p.edgeIDs[value.EdgeID], value.Key)
	}
	tuple := value.Kind + ":" + endpointKey(value.From) + ":" + endpointKey(value.To)
	p.edgeTuples[tuple] = append(p.edgeTuples[tuple], value.Key)
	return nil
}
func endpointKey(endpoint Endpoint) string {
	if endpoint.Ref != nil {
		return endpoint.Ref.Key()
	}
	return digest(endpoint.Native)
}
func digest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

func (p *projection) finish() {
	p.edgeConflicts(p.edgeIDs, "edge-id-conflict")
	p.edgeConflicts(p.edgeTuples, "edge-owner-conflict")
	for key, node := range p.nodes {
		if node.Conflict {
			for _, observed := range node.Observations {
				c := p.coverage[observed.Ref.SourceBindingID]
				c.Status = "partial"
				addReason(c, "conflicted-observations")
			}
			kind := "native-observation-conflict"
			if node.Observations[0].Ref.Kind == "objective-document" {
				kind = "objective-location-conflict"
			}
			p.graph.Conflicts = append(p.graph.Conflicts, Conflict{Kind: kind, Keys: []string{key}})
		}
		sort.Slice(node.Observations, func(i, j int) bool { return digest(node.Observations[i]) < digest(node.Observations[j]) })
		p.graph.Nodes = append(p.graph.Nodes, *node)
	}
	for _, edge := range p.edges {
		p.resolve(&edge.From)
		p.resolve(&edge.To)
		if !edge.From.Resolved || !edge.To.Resolved || edge.Conflict {
			p.graph.Partial = true
		}
		p.graph.Edges = append(p.graph.Edges, *edge)
	}
	for i := range p.graph.Aliases {
		p.resolve(&p.graph.Aliases[i].Target)
		if !p.graph.Aliases[i].Target.Resolved {
			p.graph.Partial = true
		}
	}
	for _, document := range p.documents {
		p.graph.Documents = append(p.graph.Documents, document)
	}
	for _, coverage := range p.coverage {
		if coverage.Status != "complete" {
			p.graph.Partial = true
		}
		sort.Strings(coverage.Reasons)
		p.graph.Sources = append(p.graph.Sources, *coverage)
	}
	p.graph.Partial = p.graph.Partial || len(p.graph.Conflicts) > 0
	sortGraph(&p.graph)
}
func (p *projection) resolve(endpoint *Endpoint) {
	if endpoint.Ref == nil {
		return
	}
	node := p.nodes[endpoint.Ref.Key()]
	if node == nil || node.Conflict {
		return
	}
	// An objective's gaggle-wide key does not authorize following a moved binding.
	for _, observed := range node.Observations {
		if observed.Ref == *endpoint.Ref {
			endpoint.Resolved = true
		}
	}
}
func (p *projection) edgeConflicts(groups map[string][]string, kind string) {
	for _, keys := range groups {
		if len(keys) < 2 {
			continue
		}
		sort.Strings(keys)
		p.graph.Conflicts = append(p.graph.Conflicts, Conflict{Kind: kind, Keys: append([]string(nil), keys...)})
		for _, key := range keys {
			p.edges[key].Conflict = true
		}
	}
}
func sortGraph(g *Graph) {
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Key < g.Nodes[j].Key })
	sort.Slice(g.Edges, func(i, j int) bool { return g.Edges[i].Key < g.Edges[j].Key })
	sort.Slice(g.Documents, func(i, j int) bool {
		return g.Documents[i].SourceBindingID+"/"+g.Documents[i].Path < g.Documents[j].SourceBindingID+"/"+g.Documents[j].Path
	})
	sort.Slice(g.Sources, func(i, j int) bool { return g.Sources[i].SourceBindingID < g.Sources[j].SourceBindingID })
	sort.Slice(g.Conflicts, func(i, j int) bool { return digest(g.Conflicts[i]) < digest(g.Conflicts[j]) })
	sort.Slice(g.Aliases, func(i, j int) bool { return digest(g.Aliases[i]) < digest(g.Aliases[j]) })
}
func addReason(c *Coverage, reason string) {
	for _, old := range c.Reasons {
		if old == reason {
			return
		}
	}
	c.Reasons = append(c.Reasons, reason)
}

func snapshotBounds(input Snapshot) error {
	if len(input.Backlogs)+len(input.Documents) > MaxPages {
		return ErrProjectionBound
	}
	total := 0
	check := func(value any, limit int) error {
		raw, err := json.Marshal(value)
		if err != nil || len(raw) > limit {
			return ErrProjectionBound
		}
		total += len(raw)
		if total > MaxSnapshotBytes {
			return ErrProjectionBound
		}
		return nil
	}
	for _, window := range input.Backlogs {
		if err := check(window.Page, workbench.MaxBacklogPageBytes); err != nil {
			return err
		}
	}
	for _, page := range input.Documents {
		if err := check(page, workbench.MaxDocumentPageBytes); err != nil {
			return err
		}
	}
	return nil
}
