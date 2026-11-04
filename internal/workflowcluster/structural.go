// Package workflowcluster derives deterministic, privacy-safe features from
// workflow definitions and runs so populations of workflows (notably
// agent-authored child workflows) can be clustered and compared (#8302).
//
// Everything here is advisory analysis. Nothing in this package admits,
// promotes, or edits a workflow.
package workflowcluster

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workflow/ir"
)

// StructuralFeatureVersion identifies the structural feature schema. Bump it
// whenever a feature is added, removed, or computed differently so persisted
// vectors and fitted cluster models are never silently compared across schemas.
const StructuralFeatureVersion = "workflowcluster.structural/v1"

// wlBuckets is the fixed width of the hashed Weisfeiler-Lehman histogram.
const wlBuckets = 32

// wlIterations is the number of Weisfeiler-Lehman relabeling rounds.
const wlIterations = 2

// StructuralFeatures is the genotype view of one workflow definition. Node
// names, gate condition names, goals, and other free text never contribute, so
// renaming or reordering a definition cannot move it in feature space.
type StructuralFeatures struct {
	Version        string `json:"version"`
	WorkflowDigest string `json:"workflowDigest"`
	// Numeric holds named scalar features. Names are stable within Version.
	Numeric map[string]float64 `json:"numeric"`
	// Categorical holds named sets (for example the capability set). Members
	// are sorted and unique.
	Categorical map[string][]string `json:"categorical"`
	// WL is a fixed-width, hashed histogram of Weisfeiler-Lehman subtree labels
	// capturing local graph shape independent of node names.
	WL []float64 `json:"wl"`
}

// ExtractDefinition normalizes def into canonical IR and extracts features.
// For a validated child workflow proposal, pass proposal.Machine.Def.
func ExtractDefinition(def workflow.Definition) (StructuralFeatures, error) {
	doc, err := ir.Normalize(def)
	if err != nil {
		return StructuralFeatures{}, fmt.Errorf("normalize workflow for features: %w", err)
	}
	return ExtractIR(doc), nil
}

// ExtractIR extracts structural features from a canonical IR document.
func ExtractIR(doc ir.Document) StructuralFeatures {
	g := newGraph(doc)
	numeric := nodeFeatures(doc)

	edgeKinds := map[string]float64{}
	for _, edge := range doc.Edges {
		edgeKinds[edgeKind(edge)]++
	}
	numeric["edges"] = float64(len(doc.Edges))
	for _, kind := range []string{"next", "gate", "branch", "join", "failure"} {
		numeric["edges."+kind] = edgeKinds[kind]
	}
	numeric["edges.terminal"] = float64(g.terminalEdges)
	numeric["capabilities"] = float64(len(sortedUnique(doc.Permissions)))

	depth, unreachable := g.depthFromStart(doc.Start)
	numeric["graph.depth"] = float64(depth)
	numeric["graph.unreachable"] = float64(unreachable)
	numeric["graph.maxOutDegree"] = float64(g.maxOutDegree())
	cyclic, cycleNodes := g.stronglyConnected()
	numeric["graph.cycles"] = float64(cyclic)
	numeric["graph.cycleNodes"] = float64(cycleNodes)

	sideEffects := map[string]bool{}
	goobers := map[string]bool{}
	for _, node := range doc.Nodes {
		if node.SideEffect != "" {
			sideEffects[node.SideEffect] = true
		}
		if node.Task != nil && node.Task.Goober != "" {
			goobers[node.Task.Goober] = true
		}
	}

	return StructuralFeatures{
		Version:        StructuralFeatureVersion,
		WorkflowDigest: doc.Source.Digest,
		Numeric:        numeric,
		Categorical: map[string][]string{
			"capabilities": sortedUnique(doc.Permissions),
			"sideEffects":  sortedKeys(sideEffects),
			"goobers":      sortedKeys(goobers),
		},
		WL: g.weisfeilerLehman(),
	}
}

func nodeFeatures(doc ir.Document) map[string]float64 {
	numeric := map[string]float64{
		"nodes": float64(len(doc.Nodes)), "nodes.agentic": 0, "nodes.deterministic": 0,
		"nodes.gate": 0, "nodes.gate.human": 0, "nodes.parallel": 0, "nodes.childAuthoring": 0,
		"parallel.branches": 0, "parallel.maxConcurrency": 0, "retry.nodes": 0, "retry.attempts": 0,
		"timeout.nodes": 0, "timeout.totalSeconds": 0, "timeout.maxSeconds": 0, "goal.chars": 0,
	}
	for _, node := range doc.Nodes {
		if _, known := numeric["nodes."+node.Kind]; known {
			numeric["nodes."+node.Kind]++
		}
		if node.HumanGate {
			numeric["nodes.gate.human"]++
		}
		if node.Retry != nil && node.Retry.MaxAttempts > 1 {
			numeric["retry.nodes"]++
			numeric["retry.attempts"] += float64(node.Retry.MaxAttempts)
		}
		if node.Timeout > 0 {
			numeric["timeout.nodes"]++
			numeric["timeout.totalSeconds"] += float64(node.Timeout)
			numeric["timeout.maxSeconds"] = max(numeric["timeout.maxSeconds"], float64(node.Timeout))
		}
		if node.Parallelism != nil {
			numeric["parallel.branches"] += float64(node.Parallelism.Branches)
			numeric["parallel.maxConcurrency"] = max(numeric["parallel.maxConcurrency"], float64(node.Parallelism.MaxConcurrent))
		}
		if node.Task != nil {
			// Goal text is excluded; only its size contributes.
			numeric["goal.chars"] += float64(len(node.Task.Goal))
			if node.Task.ChildWorkflows != nil {
				numeric["nodes.childAuthoring"]++
			}
		}
	}
	return numeric
}

func edgeKind(edge ir.Edge) string {
	switch {
	case edge.Condition == "":
		return "next"
	case edge.Condition == "join":
		return "join"
	case edge.Condition == "failure":
		return "failure"
	case strings.HasPrefix(edge.Condition, "branch:"):
		return "branch"
	default:
		return "gate"
	}
}

type graph struct {
	labels        map[string]string
	out           map[string][]labeledEdge
	in            map[string][]labeledEdge
	terminalEdges int
}

type labeledEdge struct {
	node string
	kind string
}

func newGraph(doc ir.Document) *graph {
	g := &graph{labels: map[string]string{}, out: map[string][]labeledEdge{}, in: map[string][]labeledEdge{}}
	for _, node := range doc.Nodes {
		retry := node.Retry != nil && node.Retry.MaxAttempts > 1
		g.labels[node.Name] = fmt.Sprintf("%s|%s|h=%t|r=%t", node.Kind, node.SideEffect, node.HumanGate, retry)
	}
	for _, edge := range doc.Edges {
		if _, ok := g.labels[edge.From]; !ok {
			continue
		}
		if _, ok := g.labels[edge.To]; !ok {
			// Empty targets and pseudo-targets such as the parallel join marker
			// end a path rather than naming a node.
			g.terminalEdges++
			continue
		}
		kind := edgeKind(edge)
		g.out[edge.From] = append(g.out[edge.From], labeledEdge{node: edge.To, kind: kind})
		g.in[edge.To] = append(g.in[edge.To], labeledEdge{node: edge.From, kind: kind})
	}
	return g
}

// depthFromStart returns the largest shortest-path distance from start and
// the number of nodes unreachable from it. Both are traversal-order
// independent.
func (g *graph) depthFromStart(start string) (int, int) {
	if _, ok := g.labels[start]; !ok {
		return 0, len(g.labels)
	}
	dist := map[string]int{start: 0}
	queue := []string{start}
	depth := 0
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range g.out[current] {
			if _, seen := dist[edge.node]; seen {
				continue
			}
			dist[edge.node] = dist[current] + 1
			depth = max(depth, dist[edge.node])
			queue = append(queue, edge.node)
		}
	}
	return depth, len(g.labels) - len(dist)
}

func (g *graph) maxOutDegree() int {
	best := 0
	for _, edges := range g.out {
		best = max(best, len(edges))
	}
	return best
}

// stronglyConnected returns the number of cyclic strongly connected
// components and the number of nodes inside them (Tarjan). Self-loops count.
func (g *graph) stronglyConnected() (int, int) {
	t := tarjan{g: g, indices: map[string]int{}, low: map[string]int{}, onStack: map[string]bool{}}
	for _, name := range sortedKeys(g.labels) {
		if _, seen := t.indices[name]; !seen {
			t.visit(name)
		}
	}
	return t.components, t.nodes
}

type tarjan struct {
	g                 *graph
	index             int
	indices, low      map[string]int
	onStack           map[string]bool
	stack             []string
	components, nodes int
}

func (t *tarjan) visit(v string) {
	t.indices[v], t.low[v] = t.index, t.index
	t.index++
	t.stack = append(t.stack, v)
	t.onStack[v] = true
	selfLoop := false
	for _, edge := range t.g.out[v] {
		selfLoop = selfLoop || edge.node == v
		if _, seen := t.indices[edge.node]; !seen {
			t.visit(edge.node)
			t.low[v] = min(t.low[v], t.low[edge.node])
		} else if t.onStack[edge.node] {
			t.low[v] = min(t.low[v], t.indices[edge.node])
		}
	}
	if t.low[v] != t.indices[v] {
		return
	}
	size := 0
	for {
		top := t.stack[len(t.stack)-1]
		t.stack = t.stack[:len(t.stack)-1]
		t.onStack[top] = false
		size++
		if top == v {
			break
		}
	}
	if size > 1 || selfLoop {
		t.components++
		t.nodes += size
	}
}

// weisfeilerLehman hashes WL subtree labels (including the initial labels)
// into a fixed-width count histogram.
func (g *graph) weisfeilerLehman() []float64 {
	hist := make([]float64, wlBuckets)
	labels := make(map[string]string, len(g.labels))
	for name, label := range g.labels {
		labels[name] = label
		hist[bucket(label)]++
	}
	for range wlIterations {
		next := make(map[string]string, len(labels))
		for name, label := range labels {
			outs := neighborLabels(g.out[name], labels, ">")
			ins := neighborLabels(g.in[name], labels, "<")
			next[name] = label + "(" + strings.Join(outs, ",") + ";" + strings.Join(ins, ",") + ")"
		}
		for _, label := range next {
			hist[bucket(label)]++
		}
		labels = next
	}
	return hist
}

func neighborLabels(edges []labeledEdge, labels map[string]string, direction string) []string {
	out := make([]string, 0, len(edges))
	for _, edge := range edges {
		out = append(out, direction+edge.kind+":"+labels[edge.node])
	}
	sort.Strings(out)
	return out
}

func bucket(label string) int {
	sum := sha256.Sum256([]byte(label))
	return int(binary.BigEndian.Uint64(sum[:8]) % wlBuckets)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedUnique(values []string) []string {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return sortedKeys(set)
}
