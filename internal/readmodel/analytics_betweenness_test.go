package readmodel

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

func TestBetweennessMatchesScanReference(t *testing.T) {
	fixtures := map[string]AnalyticsGraph{
		"empty":    {},
		"isolated": {Nodes: []AnalyticsNode{{ID: "a"}, {ID: "b"}, {ID: "c"}}},
		"equal paths": {
			Nodes: []AnalyticsNode{{ID: "start"}, {ID: "a", Failure: 1}, {ID: "b", Failure: 1}, {ID: "finish"}},
			Edges: []AnalyticsEdge{{Source: "start", Target: "a"}, {Source: "start", Target: "b"}, {Source: "a", Target: "finish"}, {Source: "b", Target: "finish"}},
		},
		"weighted cyclic and disconnected": {
			Nodes: []AnalyticsNode{{ID: "start"}, {ID: "a", Failure: 0.1}, {ID: "b", Failure: 0.8}, {ID: "c", Failure: 0.5}, {ID: "finish"}, {ID: "island"}},
			Edges: []AnalyticsEdge{{Source: "start", Target: "a"}, {Source: "start", Target: "b"}, {Source: "a", Target: "c"}, {Source: "b", Target: "c"}, {Source: "c", Target: "a"}, {Source: "c", Target: "finish"}, {Source: "island", Target: "island"}},
		},
		// After the zero-failure entry node, tiny positive distances round to
		// the same value. Lexical settling order then controls which equal
		// parents are accumulated before a node is visited.
		"rounded equal distances": {
			Nodes: []AnalyticsNode{{ID: "start"}, {ID: "entry"}, {ID: "a", Failure: math.MaxFloat64}, {ID: "b", Failure: math.MaxFloat64}, {ID: "c", Failure: math.MaxFloat64}, {ID: "finish"}},
			Edges: []AnalyticsEdge{{Source: "start", Target: "entry"}, {Source: "entry", Target: "a"}, {Source: "entry", Target: "b"}, {Source: "a", Target: "b"}, {Source: "b", Target: "a"}, {Source: "a", Target: "c"}, {Source: "b", Target: "c"}, {Source: "c", Target: "finish"}},
		},
	}
	rng := rand.New(rand.NewSource(7501))
	for size := 1; size <= 32; size++ {
		graph := AnalyticsGraph{}
		weights := []float64{0, 0.1, 0.5, 1, 1e15, math.MaxFloat64}
		for i := 0; i < size; i++ {
			graph.Nodes = append(graph.Nodes, AnalyticsNode{ID: fmt.Sprintf("n%02d", i), Failure: weights[rng.Intn(len(weights))]})
		}
		for _, source := range graph.Nodes {
			for _, target := range graph.Nodes {
				if rng.Intn(5) == 0 {
					graph.Edges = append(graph.Edges, AnalyticsEdge{Source: source.ID, Target: target.ID})
				}
			}
		}
		fixtures[fmt.Sprintf("generated-%02d", size)] = graph
	}
	for name, graph := range fixtures {
		t.Run(name, func(t *testing.T) {
			ids, adj, failure := betweennessInputs(graph)
			want := scanBetweennessReference(ids, adj, failure)
			rng := rand.New(rand.NewSource(7501))
			for permutation := 0; permutation < 5; permutation++ {
				rng.Shuffle(len(graph.Nodes), func(i, j int) { graph.Nodes[i], graph.Nodes[j] = graph.Nodes[j], graph.Nodes[i] })
				rng.Shuffle(len(graph.Edges), func(i, j int) { graph.Edges[i], graph.Edges[j] = graph.Edges[j], graph.Edges[i] })
				got, err := AnalyzeGraph(graph)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Centrality, want) {
					t.Fatalf("permutation %d centrality = %+v, want scan result %+v", permutation, got.Centrality, want)
				}
			}
		})
	}
}

func betweennessInputs(graph AnalyticsGraph) (map[string]bool, map[string][]string, map[string]float64) {
	ids := make(map[string]bool, len(graph.Nodes))
	failure := make(map[string]float64, len(graph.Nodes))
	adj := make(map[string][]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		ids[node.ID] = true
		failure[node.ID] = node.Failure
	}
	for _, edge := range graph.Edges {
		adj[edge.Source] = append(adj[edge.Source], edge.Target)
	}
	for id := range adj {
		sort.Strings(adj[id])
	}
	return ids, adj, failure
}

func BenchmarkBetweennessSparse(b *testing.B) {
	for _, size := range []int{32, 128, 512} {
		graph := AnalyticsGraph{}
		for i := 0; i < size; i++ {
			id := fmt.Sprintf("n%04d", i)
			graph.Nodes = append(graph.Nodes, AnalyticsNode{ID: id, Failure: float64(i%5+1) / 5})
			for _, step := range []int{1, 7} {
				if i+step < size {
					graph.Edges = append(graph.Edges, AnalyticsEdge{Source: id, Target: fmt.Sprintf("n%04d", i+step)})
				}
			}
		}
		ids, adj, failure := betweennessInputs(graph)
		b.Run(fmt.Sprintf("nodes=%d", size), func(b *testing.B) {
			for _, algorithm := range []struct {
				name string
				run  func(map[string]bool, map[string][]string, map[string]float64) []CentralityScore
			}{
				{"scan", scanBetweennessReference},
				{"heap", betweenness},
			} {
				b.Run(algorithm.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						algorithm.run(ids, adj, failure)
					}
				})
			}
		})
	}
}

// scanBetweennessReference retains the pre-heap algorithm as an exact oracle.
// In particular, keep its distance comparison and accumulation order unchanged.
func scanBetweennessReference(ids map[string]bool, adj map[string][]string, failure map[string]float64) []CentralityScore {
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
