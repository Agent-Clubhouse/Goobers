// Package blockedcycle detects circular dependencies between blocked backlog
// items and renders the provider comments that report them.
//
// Callers hold learned dependency blocks in whatever shape suits them (the
// runner keys them by repository and item id in blocked.json); they hand this
// package a normalized, deterministically ordered []Record and it answers
// which items form a cycle, which representative paths describe it, and what
// comment text escalates it.
package blockedcycle

import (
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/goobers/goobers/providers"
)

// MaxPaths caps how many representative shortest paths a Result carries, so a
// dense graph cannot trigger factorial path enumeration.
const MaxPaths = 3

// Node is one item in the dependency graph: a repository-scoped item id.
type Node struct {
	Repository providers.RepositoryRef
	ItemID     string
}

// Record is one learned dependency block: the item and the ids it is blocked
// on. Item ids must already be normalized the way the caller's provider
// lookups expect them, and records must be supplied in a deterministic order.
type Record struct {
	Repository providers.RepositoryRef
	ItemID     string
	Blockers   []string
}

// Result describes one cycle: every affected member (the seed node first),
// representative paths through it, and whether further paths exist.
type Result struct {
	Affected  []Node
	Paths     [][]string
	MorePaths bool
}

type edge struct {
	From Node
	To   Node
}

// BuildGraph turns records into a forward graph (item -> its blockers) and the
// corresponding reverse graph, shared by Find (one node's SCC) and FindAll
// (every SCC in the current state). Graph keys are only items that carry their
// own record; a node referenced solely as someone else's blocker (never itself
// recorded, e.g. an external one-way dependency) has no outgoing edges and so
// can never itself seed or complete a cycle.
func BuildGraph(records []Record) (graph, reverseGraph map[Node][]Node) {
	graph = make(map[Node][]Node, len(records))
	edgeSeen := make(map[Node]map[Node]bool, len(records))
	for _, rec := range records {
		if RepositoryEmpty(rec.Repository) {
			continue
		}
		node := Node{Repository: rec.Repository, ItemID: rec.ItemID}
		if _, ok := graph[node]; !ok {
			graph[node] = nil
		}
		if edgeSeen[node] == nil {
			edgeSeen[node] = make(map[Node]bool)
		}
		for _, blockerID := range rec.Blockers {
			blocker := Node{Repository: rec.Repository, ItemID: blockerID}
			if !edgeSeen[node][blocker] {
				graph[node] = append(graph[node], blocker)
				edgeSeen[node][blocker] = true
			}
		}
	}

	reverseGraph = make(map[Node][]Node, len(graph))
	for from, edges := range graph {
		if _, ok := reverseGraph[from]; !ok {
			reverseGraph[from] = nil
		}
		for _, to := range edges {
			reverseGraph[to] = append(reverseGraph[to], from)
		}
	}
	return graph, reverseGraph
}

// Find identifies the strongly connected component containing item.
// Forward/reverse reachability is linear in graph size; representative
// shortest paths are capped at MaxPaths so dense graphs cannot trigger
// factorial path enumeration.
func Find(records []Record, item Node) Result {
	if RepositoryEmpty(item.Repository) {
		return Result{}
	}
	graph, reverseGraph := BuildGraph(records)
	return resultForNode(graph, reverseGraph, item)
}

// FindAll enumerates every strongly connected component of size >1 (or
// self-loop) currently present in records — every active cycle, not just the
// one touching a single just-written record.
//
// Why this exists (#1405): Find answers "is THIS item's write part of a cycle
// right now", which is correct at the instant it runs but only runs when that
// item's own blocked handler fires — and a fully skip-parked cycle member is
// never reclaimed again by design (#552), so its handler never re-fires to
// notice a cycle sibling whose escalation later drifted (a human override, a
// stale re-curation pass, anything that reset one member's labels without any
// new blocked-record write). Reconciliation against that drift has to be
// driven from something that runs on every tick regardless of claim state, so
// it needs the full current cycle set, not one node's.
func FindAll(records []Record) []Result {
	graph, reverseGraph := BuildGraph(records)

	nodes := make([]Node, 0, len(graph))
	for node := range graph {
		nodes = append(nodes, node)
	}
	sortNodes(nodes)

	seen := make(map[Node]bool, len(nodes))
	var cycles []Result
	for _, node := range nodes {
		if seen[node] {
			continue
		}
		result := resultForNode(graph, reverseGraph, node)
		if len(result.Affected) == 0 {
			seen[node] = true
			continue
		}
		for _, member := range result.Affected {
			seen[member] = true
		}
		cycles = append(cycles, result)
	}
	return cycles
}

// resultForNode computes item's strongly connected component (forward
// reachability ∩ backward reachability) and, when that component is a real
// cycle (size >1, or a direct self-loop), the affected member list and the
// representative paths Comments reports.
func resultForNode(graph, reverseGraph map[Node][]Node, item Node) Result {
	forward := reachable(graph, item)
	backward := reachable(reverseGraph, item)

	component := make(map[Node]bool)
	for node := range forward {
		if backward[node] {
			component[node] = true
		}
	}
	if len(component) == 1 && !slices.Contains(graph[item], item) {
		return Result{}
	}

	affected := []Node{item}
	var others []Node
	for node := range component {
		if node != item {
			others = append(others, node)
		}
	}
	sortNodes(others)
	affected = append(affected, others...)

	var paths [][]string
	coveredNodes := map[Node]bool{item: true}
	coveredEdges := make(map[edge]bool)
	appendPath := func(nodes []Node) {
		path := make([]string, len(nodes))
		for i, node := range nodes {
			path[i] = node.ItemID
			coveredNodes[node] = true
			if i > 0 {
				coveredEdges[edge{From: nodes[i-1], To: node}] = true
			}
		}
		paths = append(paths, path)
	}

	if len(component) == 1 {
		appendPath([]Node{item, item})
	} else {
		for _, member := range affected[1:] {
			if coveredNodes[member] || len(paths) == MaxPaths {
				continue
			}
			outbound, found := shortestPath(graph, item, member, component)
			if !found {
				continue
			}
			inbound, found := shortestPath(graph, member, item, component)
			if !found {
				continue
			}
			appendPath(append(outbound, inbound[1:]...))
		}
	}

	morePaths := false
	for node := range component {
		if !coveredNodes[node] {
			morePaths = true
			break
		}
	}
	if !morePaths {
		for from, edges := range graph {
			if !component[from] {
				continue
			}
			for _, to := range edges {
				if component[to] && !coveredEdges[edge{From: from, To: to}] {
					morePaths = true
					break
				}
			}
			if morePaths {
				break
			}
		}
	}
	return Result{Affected: affected, Paths: paths, MorePaths: morePaths}
}

func reachable(graph map[Node][]Node, start Node) map[Node]bool {
	reached := map[Node]bool{start: true}
	queue := []Node{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range graph[node] {
			if reached[next] {
				continue
			}
			reached[next] = true
			queue = append(queue, next)
		}
	}
	return reached
}

func shortestPath(graph map[Node][]Node, start, target Node, allowed map[Node]bool) ([]Node, bool) {
	if start == target {
		return []Node{target}, true
	}
	seen := map[Node]bool{start: true}
	previous := make(map[Node]Node)
	queue := []Node{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range graph[node] {
			if !allowed[next] || seen[next] {
				continue
			}
			seen[next] = true
			previous[next] = node
			if next == target {
				path := []Node{target}
				for current := target; current != start; {
					current = previous[current]
					path = append(path, current)
				}
				slices.Reverse(path)
				return path, true
			}
			queue = append(queue, next)
		}
	}
	return nil, false
}

// RepositoryIdentity renders a repository reference as a stable, escaped
// identity string, used both for deterministic ordering here and for the
// caller's own record keys.
func RepositoryIdentity(repo providers.RepositoryRef) string {
	if RepositoryEmpty(repo) {
		return ""
	}
	parts := []string{string(repo.Provider), repo.Owner}
	if repo.Project != "" {
		parts = append(parts, repo.Project)
	}
	parts = append(parts, repo.Name)
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// RepositoryEmpty reports whether repo carries no repository scoping at all.
func RepositoryEmpty(repo providers.RepositoryRef) bool {
	return repo.Provider == "" && repo.Owner == "" && repo.Project == "" && repo.Name == ""
}

func sortNodes(nodes []Node) {
	sort.Slice(nodes, func(i, j int) bool {
		if left, right := RepositoryIdentity(nodes[i].Repository), RepositoryIdentity(nodes[j].Repository); left != right {
			return left < right
		}
		return nodes[i].ItemID < nodes[j].ItemID
	})
}
