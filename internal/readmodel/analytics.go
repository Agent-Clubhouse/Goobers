package readmodel

import (
	"fmt"
	"sort"
)

// AnalyticsGraph is the weighted topology consumed by graph analytics. Node
// weights are failure shares for centrality and stage latency for the critical
// path.
type AnalyticsGraph struct {
	Nodes []AnalyticsNode
	Edges []AnalyticsEdge
}

// AnalyticsNode carries the outcome and latency weights for one stage.
type AnalyticsNode struct {
	ID      string  `json:"id"`
	Failure float64 `json:"failure"`
	Latency float64 `json:"latency"`
}

// AnalyticsEdge is a directed transition between workflow stages.
type AnalyticsEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// GraphAnalytics contains the computed topology insights.
type GraphAnalytics struct {
	Centrality   []CentralityScore `json:"centrality"`
	CriticalPath CriticalPath      `json:"criticalPath"`
	Cycles       [][]string        `json:"cycles"`
	Confidence   string            `json:"confidence"`
	Caveat       string            `json:"caveat,omitempty"`
}

// CentralityScore ranks a node by weighted betweenness.
type CentralityScore struct {
	Node  string  `json:"node"`
	Score float64 `json:"score"`
}

// CriticalPath is the highest-latency path through an acyclic graph.
type CriticalPath struct {
	Nodes  []string `json:"nodes"`
	Weight float64  `json:"weight"`
}

// AnalyzeGraph returns weighted betweenness blame, the longest DAG path, and
// strongly connected components that contain a cycle.
func AnalyzeGraph(graph AnalyticsGraph) (GraphAnalytics, error) {
	ids := make(map[string]bool, len(graph.Nodes))
	failure := make(map[string]float64, len(graph.Nodes))
	latency := make(map[string]float64, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if node.ID == "" {
			return GraphAnalytics{}, fmt.Errorf("readmodel: graph node has empty id")
		}
		if ids[node.ID] {
			return GraphAnalytics{}, fmt.Errorf("readmodel: duplicate graph node %q", node.ID)
		}
		ids[node.ID] = true
		failure[node.ID] = nonNegative(node.Failure)
		latency[node.ID] = nonNegative(node.Latency)
	}
	adj := make(map[string][]string, len(ids))
	edges := make(map[string]bool, len(graph.Edges))
	for _, edge := range graph.Edges {
		if !ids[edge.Source] || !ids[edge.Target] {
			return GraphAnalytics{}, fmt.Errorf("readmodel: graph edge %q -> %q references an unknown node", edge.Source, edge.Target)
		}
		key := edge.Source + "\x00" + edge.Target
		if edges[key] {
			continue
		}
		edges[key] = true
		adj[edge.Source] = append(adj[edge.Source], edge.Target)
	}
	for id := range adj {
		sort.Strings(adj[id])
	}

	result := GraphAnalytics{
		Centrality: betweenness(ids, adj, failure),
		Cycles:     stronglyConnectedCycles(ids, adj),
	}
	if len(result.Cycles) == 0 {
		result.CriticalPath = longestPath(ids, adj, latency)
	}
	return result, nil
}

func nonNegative(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

// Brandes' algorithm uses inverse failure as distance: high-failure routes are
// the paths on which blame centrality is most useful.
func betweenness(ids map[string]bool, adj map[string][]string, failure map[string]float64) []CentralityScore {
	scores := make(map[string]float64, len(ids))
	nodes := sortedIDs(ids)
	for _, source := range nodes {
		dist := map[string]float64{source: 0}
		sigma := map[string]float64{source: 1}
		parents := make(map[string][]string, len(ids))
		visited := make(map[string]bool, len(ids))
		order := make([]string, 0, len(ids))
		for {
			current := ""
			for node, distance := range dist {
				if !visited[node] && (current == "" || distance < dist[current] ||
					(distance == dist[current] && node < current)) {
					current = node
				}
			}
			if current == "" {
				break
			}
			visited[current] = true
			order = append(order, current)
			for _, next := range adj[current] {
				distance := 1 / (failure[next] + 1e-9)
				candidate := dist[current] + distance
				known, seen := dist[next]
				if !seen || candidate < known-1e-12 {
					dist[next] = candidate
					sigma[next] = sigma[current]
					parents[next] = []string{current}
				} else if !visited[next] && candidate == known {
					sigma[next] += sigma[current]
					parents[next] = append(parents[next], current)
				}
			}
		}
		dependency := make(map[string]float64, len(ids))
		for i := len(order) - 1; i >= 0; i-- {
			node := order[i]
			for _, parent := range parents[node] {
				dependency[parent] += (sigma[parent] / sigma[node]) * (1 + dependency[node])
			}
			if node != source {
				scores[node] += dependency[node] * (failure[node] + 1)
			}
		}
	}
	result := make([]CentralityScore, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, CentralityScore{Node: node, Score: scores[node]})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		return result[i].Node < result[j].Node
	})
	return result
}

func longestPath(ids map[string]bool, adj map[string][]string, latency map[string]float64) CriticalPath {
	indegree := make(map[string]int, len(ids))
	for _, targets := range adj {
		for _, target := range targets {
			indegree[target]++
		}
	}
	queue := make([]string, 0)
	for _, id := range sortedIDs(ids) {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	distance := make(map[string]float64, len(ids))
	paths := make(map[string][]string, len(ids))
	for _, id := range sortedIDs(ids) {
		distance[id] = latency[id]
		paths[id] = []string{id}
	}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range adj[node] {
			candidate := distance[node] + latency[next]
			if candidate > distance[next] || (candidate == distance[next] && pathLess(paths[node], paths[next], next)) {
				distance[next] = candidate
				paths[next] = append(append([]string{}, paths[node]...), next)
			}
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
				sort.Strings(queue)
			}
		}
	}
	best := CriticalPath{}
	for _, id := range sortedIDs(ids) {
		if distance[id] > best.Weight || (distance[id] == best.Weight && pathLess(paths[id], best.Nodes, "")) {
			best = CriticalPath{Nodes: paths[id], Weight: distance[id]}
		}
	}
	return best
}

func pathLess(left, right []string, suffix string) bool {
	if suffix != "" {
		left = append(append([]string{}, left...), suffix)
	}
	if len(right) == 0 {
		return true
	}
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return len(left) < len(right)
}

func stronglyConnectedCycles(ids map[string]bool, adj map[string][]string) [][]string {
	index, next := make(map[string]int, len(ids)), 0
	low, stack := make(map[string]int, len(ids)), []string{}
	onStack := make(map[string]bool, len(ids))
	var components [][]string
	var visit func(string)
	visit = func(node string) {
		index[node], low[node] = next, next
		next++
		stack = append(stack, node)
		onStack[node] = true
		for _, target := range adj[node] {
			if _, seen := index[target]; !seen {
				visit(target)
				if low[target] < low[node] {
					low[node] = low[target]
				}
			} else if onStack[target] && index[target] < low[node] {
				low[node] = index[target]
			}
		}
		if low[node] != index[node] {
			return
		}
		var component []string
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, last)
			if last == node {
				break
			}
		}
		selfLoop := false
		for _, target := range adj[node] {
			if target == node {
				selfLoop = true
				break
			}
		}
		if len(component) > 1 || selfLoop {
			sort.Strings(component)
			components = append(components, component)
		}
	}
	for _, id := range sortedIDs(ids) {
		if _, seen := index[id]; !seen {
			visit(id)
		}
	}
	sort.Slice(components, func(i, j int) bool { return components[i][0] < components[j][0] })
	return components
}

func sortedIDs(ids map[string]bool) []string {
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
