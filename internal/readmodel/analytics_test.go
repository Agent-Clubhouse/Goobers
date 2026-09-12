package readmodel

import (
	"encoding/json"
	"testing"
)

func TestAnalyzeGraphFindsWeightedCriticalPathAndCentrality(t *testing.T) {
	graph := AnalyticsGraph{
		Nodes: []AnalyticsNode{
			{ID: "start", Latency: 1},
			{ID: "shared", Failure: 0.8, Latency: 3},
			{ID: "finish", Latency: 1},
			{ID: "alternate", Latency: 2},
		},
		Edges: []AnalyticsEdge{
			{Source: "start", Target: "shared"},
			{Source: "shared", Target: "finish"},
			{Source: "start", Target: "alternate"},
			{Source: "alternate", Target: "finish"},
		},
	}
	result, err := AnalyzeGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result.CriticalPath.Nodes, []string{"start", "shared", "finish"}; len(got) != len(want) || got[1] != want[1] {
		t.Fatalf("critical path = %v, want %v", got, want)
	}
	if result.CriticalPath.Weight != 5 {
		t.Fatalf("critical path weight = %v, want 5", result.CriticalPath.Weight)
	}
	if result.Centrality[0].Node != "shared" || result.Centrality[0].Score <= 0 {
		t.Fatalf("centrality = %+v, want shared ranked first", result.Centrality)
	}
}

func TestAnalyzeGraphFindsStronglyConnectedCycles(t *testing.T) {
	result, err := AnalyzeGraph(AnalyticsGraph{
		Nodes: []AnalyticsNode{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		Edges: []AnalyticsEdge{
			{Source: "a", Target: "b"},
			{Source: "b", Target: "a"},
			{Source: "b", Target: "c"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Cycles) != 1 || len(result.Cycles[0]) != 2 {
		t.Fatalf("cycles = %v, want [[a b]]", result.Cycles)
	}
	if len(result.CriticalPath.Nodes) != 0 {
		t.Fatalf("critical path on cyclic graph = %+v, want empty", result.CriticalPath)
	}
}

func TestAnalyzeGraphDeduplicatesParallelDeclaredEdges(t *testing.T) {
	graph := AnalyticsGraph{
		Nodes: []AnalyticsNode{
			{ID: "start"},
			{ID: "shared", Failure: 0.5},
			{ID: "finish"},
		},
		Edges: []AnalyticsEdge{
			{Source: "start", Target: "shared"},
			{Source: "start", Target: "shared"},
			{Source: "shared", Target: "finish"},
		},
	}
	duplicate, err := AnalyzeGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	graph.Edges = graph.Edges[:1]
	graph.Edges = append(graph.Edges, AnalyticsEdge{Source: "shared", Target: "finish"})
	unique, err := AnalyzeGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	if len(duplicate.Centrality) != len(unique.Centrality) {
		t.Fatalf("centrality lengths = %d / %d", len(duplicate.Centrality), len(unique.Centrality))
	}
	for i := range unique.Centrality {
		if duplicate.Centrality[i] != unique.Centrality[i] {
			t.Fatalf("duplicate edge changed centrality: %+v / %+v", duplicate.Centrality, unique.Centrality)
		}
	}
	if duplicate.CriticalPath.Weight != unique.CriticalPath.Weight {
		t.Fatalf("duplicate edge changed critical path: %+v / %+v", duplicate.CriticalPath, unique.CriticalPath)
	}
}

// TestAnalyzeGraphNeverReturnsNilSlices is #4825's core regression: Centrality,
// Cycles, and CriticalPath.Nodes have no `omitempty` and no nullable
// TypeScript type on the client, so a nil Go slice marshals to JSON null and
// crashes the portal. An empty graph is the extreme case (no nodes, so
// betweenness/longestPath/stronglyConnectedCycles all have nothing to loop
// over), but a cyclic graph also leaves CriticalPath unset by AnalyzeGraph.
func TestAnalyzeGraphNeverReturnsNilSlices(t *testing.T) {
	for name, graph := range map[string]AnalyticsGraph{
		"empty": {},
		"single node, no edges": {
			Nodes: []AnalyticsNode{{ID: "solo"}},
		},
		"cyclic": {
			Nodes: []AnalyticsNode{{ID: "a"}, {ID: "b"}},
			Edges: []AnalyticsEdge{{Source: "a", Target: "b"}, {Source: "b", Target: "a"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := AnalyzeGraph(graph)
			if err != nil {
				t.Fatal(err)
			}
			if result.Centrality == nil {
				t.Fatal("Centrality is nil, want a non-nil (possibly empty) slice")
			}
			if result.Cycles == nil {
				t.Fatal("Cycles is nil, want a non-nil (possibly empty) slice")
			}
			if result.CriticalPath.Nodes == nil {
				t.Fatal("CriticalPath.Nodes is nil, want a non-nil (possibly empty) slice")
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"centrality", "cycles"} {
				if string(decoded[field]) == "null" {
					t.Fatalf("%s marshaled as JSON null, want []", field)
				}
			}
			var criticalPath map[string]json.RawMessage
			if err := json.Unmarshal(decoded["criticalPath"], &criticalPath); err != nil {
				t.Fatal(err)
			}
			if string(criticalPath["nodes"]) == "null" {
				t.Fatal("criticalPath.nodes marshaled as JSON null, want []")
			}
		})
	}
}
