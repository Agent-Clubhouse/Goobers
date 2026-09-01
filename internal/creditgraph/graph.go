package creditgraph

// SchemaVersion identifies the graph contract's shape. Readers branch on it.
const SchemaVersion = "goobers.dev/creditgraph/v1"

// NodeKind is the type of a graph node. The taxonomy is the contract: a
// consumer switches on it, so values are stable, dotted-free strings.
type NodeKind string

// The node taxonomy.
const (
	// KindOutcome is the run's final outcome — the graph's root.
	KindOutcome NodeKind = "outcome"
	// KindRun is one run of one workflow.
	KindRun NodeKind = "run"
	// KindStage is one stage attempt within a run.
	KindStage NodeKind = "stage"
	// KindSubagent is one nested agent invocation within a stage.
	KindSubagent NodeKind = "subagent"
	// KindModelInvocation is one model call made by a subagent.
	KindModelInvocation NodeKind = "model-invocation"
	// KindToolCall is one tool call issued during a model invocation.
	KindToolCall NodeKind = "tool-call"
	// KindToolResult is the observed result of a tool call.
	KindToolResult NodeKind = "tool-result"
	// KindTool is the tool identity shared by every call that names it.
	KindTool NodeKind = "tool"
	// KindEvidence is an artifact a stage committed by content digest.
	KindEvidence NodeKind = "evidence"
	// KindEvaluator is a gate evaluation that judged the run.
	KindEvaluator NodeKind = "evaluator"
)

// EdgeKind is the type of a directed edge. Every edge points from the
// containing or causing element toward the nested or caused one, so the whole
// graph is traversable downward from the outcome.
type EdgeKind string

// The edge taxonomy.
const (
	// EdgeAttributedTo joins the final outcome to the run it summarizes.
	EdgeAttributedTo EdgeKind = "attributed-to"
	// EdgeContains joins a parent execution element to a nested one.
	EdgeContains EdgeKind = "contains"
	// EdgeDelegates joins a coordinating subagent to a subagent it spawned.
	EdgeDelegates EdgeKind = "delegates"
	// EdgeInvokes joins a model invocation to a tool call it issued.
	EdgeInvokes EdgeKind = "invokes"
	// EdgeUses joins a tool call to the shared tool identity it names.
	EdgeUses EdgeKind = "uses"
	// EdgeProduces joins an element to the result or evidence it produced.
	EdgeProduces EdgeKind = "produces"
	// EdgeEvaluates joins an evaluator to what it judged.
	EdgeEvaluates EdgeKind = "evaluates"
	// EdgeDependsOn joins a subagent to a subagent it declared a dependency on.
	EdgeDependsOn EdgeKind = "depends-on"
)

// Provenance grades how a node or edge came to be in the graph.
type Provenance string

// Provenance grades.
const (
	// ProvenanceRecorded means the journal states this node or edge directly.
	ProvenanceRecorded Provenance = "recorded"
	// ProvenanceUnknown means the element is referenced but its own record is
	// absent. It is a placeholder for missing provenance, never an inference.
	ProvenanceUnknown Provenance = "unknown"
)

// Node is one element of the execution graph.
type Node struct {
	ID         string            `json:"id"`
	Kind       NodeKind          `json:"kind"`
	Label      string            `json:"label,omitempty"`
	Stage      string            `json:"stage,omitempty"`
	Attempt    int               `json:"attempt,omitempty"`
	Provenance Provenance        `json:"provenance"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Edge is one directed relationship between two nodes.
type Edge struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Kind       EdgeKind   `json:"kind"`
	Provenance Provenance `json:"provenance"`
}

// Gap records one piece of provenance the journal did not carry. Gaps are the
// graph's explicit statement of what it does not know.
type Gap struct {
	NodeID string   `json:"nodeId"`
	Kind   NodeKind `json:"kind"`
	Reason string   `json:"reason"`
}

// Graph is the materialized execution/credit graph for one run. Nodes, edges,
// and gaps are in deterministic construction order.
type Graph struct {
	Schema   string `json:"schema"`
	RunID    string `json:"runId,omitempty"`
	Gaggle   string `json:"gaggle,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	RootID   string `json:"rootId"`
	Nodes    []Node `json:"nodes"`
	Edges    []Edge `json:"edges"`
	Gaps     []Gap  `json:"gaps,omitempty"`

	index    map[string]int
	children map[string][]int
}

// Node returns the node with the given id.
func (g *Graph) Node(id string) (Node, bool) {
	if g == nil {
		return Node{}, false
	}
	position, ok := g.index[id]
	if !ok {
		return Node{}, false
	}
	return g.Nodes[position], true
}

// Root returns the graph's root, the run's final outcome.
func (g *Graph) Root() (Node, bool) {
	if g == nil {
		return Node{}, false
	}
	return g.Node(g.RootID)
}

// OutEdges returns the edges leaving a node, in construction order.
func (g *Graph) OutEdges(id string) []Edge {
	if g == nil {
		return nil
	}
	positions := g.children[id]
	edges := make([]Edge, 0, len(positions))
	for _, position := range positions {
		edges = append(edges, g.Edges[position])
	}
	return edges
}

// Children returns the nodes an edge leads to from id, in construction order.
// A node reachable by more than one edge — a shared tool, for instance —
// appears once per edge.
func (g *Graph) Children(id string) []Node {
	edges := g.OutEdges(id)
	nodes := make([]Node, 0, len(edges))
	for _, edge := range edges {
		if node, ok := g.Node(edge.To); ok {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// Walk performs a depth-first traversal from id, visiting each reachable node
// once even when it is shared by several parents. Visiting stops descending
// below a node when visit returns false. Depth is 0 at the starting node.
func (g *Graph) Walk(id string, visit func(node Node, depth int) bool) {
	if g == nil || visit == nil {
		return
	}
	seen := make(map[string]bool, len(g.Nodes))
	g.walk(id, 0, seen, visit)
}

func (g *Graph) walk(id string, depth int, seen map[string]bool, visit func(Node, int) bool) {
	if seen[id] {
		return
	}
	node, ok := g.Node(id)
	if !ok {
		return
	}
	seen[id] = true
	if !visit(node, depth) {
		return
	}
	for _, edge := range g.OutEdges(id) {
		g.walk(edge.To, depth+1, seen, visit)
	}
}

// Descendants returns every node reachable from id, excluding id itself, in
// depth-first construction order.
func (g *Graph) Descendants(id string) []Node {
	var result []Node
	g.Walk(id, func(node Node, depth int) bool {
		if depth > 0 {
			result = append(result, node)
		}
		return true
	})
	return result
}

// NodesOfKind returns every node of one kind, in construction order.
func (g *Graph) NodesOfKind(kind NodeKind) []Node {
	if g == nil {
		return nil
	}
	var result []Node
	for _, node := range g.Nodes {
		if node.Kind == kind {
			result = append(result, node)
		}
	}
	return result
}
