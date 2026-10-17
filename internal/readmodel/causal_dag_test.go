package readmodel

import (
	"fmt"
	"maps"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/workflow"
)

func TestBuildCausalDAG(t *testing.T) {
	tests := []struct {
		name  string
		nodes map[string]causalNodeFact
		graph *workflow.Graph
		want  map[string][]string
	}{
		{name: "empty run"},
		{
			name: "isolated node clears stale descendants",
			nodes: map[string]causalNodeFact{
				"stage:alone": {causalDAG: map[string]struct{}{"stage:stale": {}}},
			},
			want: map[string][]string{"stage:alone": {}},
		},
		{
			name: "observed chain keeps only immediate children",
			nodes: map[string]causalNodeFact{
				"stage:first":  {identity: "v1"},
				"stage:second": {parents: []string{"stage:first", "stage:first", "stage:missing"}, randomized: true, arm: "treatment"},
				"stage:third":  {parents: []string{"stage:second"}},
			},
			want: map[string][]string{
				"stage:first": {"stage:second"}, "stage:second": {"stage:third"}, "stage:third": {},
			},
		},
		{
			name: "fan-out and fan-in",
			nodes: map[string]causalNodeFact{
				"stage:root":  {},
				"stage:left":  {parents: []string{"stage:root"}},
				"stage:right": {parents: []string{"stage:root"}},
				"gate:join":   {parents: []string{"stage:left", "stage:right"}},
			},
			want: map[string][]string{
				"stage:root": {"stage:left", "stage:right"}, "stage:left": {"gate:join"},
				"stage:right": {"gate:join"}, "gate:join": {},
			},
		},
		{
			name: "self edges and cycles retain immediate edges",
			nodes: map[string]causalNodeFact{
				"stage:a": {parents: []string{"stage:a", "stage:b"}},
				"stage:b": {parents: []string{"stage:a"}},
			},
			want: map[string][]string{"stage:a": {"stage:a", "stage:b"}, "stage:b": {"stage:a"}},
		},
		{
			name: "declared and observed edges use only run nodes",
			nodes: map[string]causalNodeFact{
				"stage:start": {},
				"stage:work":  {parents: []string{"stage:start", "stage:extra", "stage:absent"}},
				"stage:extra": {},
				"gate:check":  {},
			},
			graph: &workflow.Graph{
				Nodes: []workflow.GraphNode{
					{ID: "start", Kind: workflow.GraphNodeDeterministic},
					{ID: "work", Kind: workflow.GraphNodeAgentic},
					{ID: "extra", Kind: workflow.GraphNodeAgentic},
					{ID: "check", Kind: workflow.GraphNodeGate},
					{ID: "absent", Kind: workflow.GraphNodeAgentic},
					{ID: "unrouted", Kind: workflow.GraphNodeAgentic},
					{ID: "parallel", Kind: workflow.GraphNodeParallel},
				},
				Edges: []workflow.GraphEdge{
					{Source: "start", Target: "work"},
					{Source: "start", Target: "work"},
					{Source: "work", Target: "check"},
					{Source: "absent", Target: "work"},
					{Source: "start", Target: "unrouted"},
					{Source: "work", Target: "extra", Terminal: workflow.GraphTerminalComplete},
					{Source: "parallel", Target: "work"},
					{Source: "work", Target: "parallel"},
					{Source: "unknown", Target: "work"},
					{Source: "work", Target: "unknown"},
				},
			},
			want: map[string][]string{
				"stage:start": {"stage:work"}, "stage:work": {"gate:check"},
				"stage:extra": {"stage:work"}, "gate:check": {},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := maps.Clone(tt.nodes)
			for node, fact := range want {
				fact.causalDAG = make(map[string]struct{})
				for _, child := range tt.want[node] {
					fact.causalDAG[child] = struct{}{}
				}
				want[node] = fact
			}
			// Rebuild from differently iterated maps, and rebuild an already
			// populated run, to check that sets are independent of map order.
			for range 10 {
				run := &causalRunFact{nodes: maps.Clone(tt.nodes)}
				for range 2 {
					if err := buildCausalDAG([]*causalRunFact{run}, tt.graph); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(run.nodes, want) {
						t.Fatalf("nodes = %#v, want %#v", run.nodes, want)
					}
				}
			}
		})
	}
}

func TestBuildCausalDAGKeepsRunsIndependent(t *testing.T) {
	runs := []*causalRunFact{
		{nodes: map[string]causalNodeFact{"stage:a": {}, "stage:b": {parents: []string{"stage:a"}}}},
		{nodes: map[string]causalNodeFact{"stage:a": {}, "stage:b": {}}},
	}
	if err := buildCausalDAG(runs, nil); err != nil {
		t.Fatal(err)
	}
	if got := runs[0].nodes["stage:a"].causalDAG; !reflect.DeepEqual(got, map[string]struct{}{"stage:b": {}}) {
		t.Fatalf("first run descendants = %v, want stage:b", got)
	}
	if got := runs[1].nodes["stage:a"].causalDAG; got == nil || len(got) != 0 {
		t.Fatalf("second run descendants = %v, want non-nil empty set", got)
	}
}

func BenchmarkBuildCausalDAG(b *testing.B) {
	for _, size := range []int{64, 256, 1024} {
		b.Run(fmt.Sprintf("nodes=%d", size), func(b *testing.B) {
			nodes := make(map[string]causalNodeFact, size)
			for i := range size {
				var parents []string
				for _, offset := range []int{1, 7} {
					if i >= offset {
						parents = append(parents, fmt.Sprintf("stage:%04d", i-offset))
					}
				}
				nodes[fmt.Sprintf("stage:%04d", i)] = causalNodeFact{parents: parents}
			}
			runs := []*causalRunFact{{nodes: nodes}}
			b.ReportAllocs()
			for b.Loop() {
				if err := buildCausalDAG(runs, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
