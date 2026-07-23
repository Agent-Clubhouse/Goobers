package model

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestMachineGraphIsPinnedAndReadOnly(t *testing.T) {
	def := Definition{Name: "compiled", Version: 3}
	source := Graph{
		Start: "interpreted-start",
		Nodes: []GraphNode{{ID: "interpreted-start", Kind: GraphNodeAgentic}},
		Edges: []GraphEdge{{Source: "interpreted-start", Terminal: GraphTerminalComplete}},
	}
	machine, err := NewMachine(
		def,
		map[string]apiv1.Task{},
		map[string]apiv1.Gate{},
		source,
	)
	if err != nil {
		t.Fatal(err)
	}

	want := Graph{
		Name:    def.Name,
		Version: def.Version,
		Digest:  machine.Digest(),
		Start:   "interpreted-start",
		Nodes:   []GraphNode{{ID: "interpreted-start", Kind: GraphNodeAgentic}},
		Edges:   []GraphEdge{{Source: "interpreted-start", Terminal: GraphTerminalComplete}},
	}
	source.Nodes[0].ID = "mutated-source"
	source.Edges[0].Target = "mutated-source"
	if got := machine.Graph(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Graph() after source mutation = %+v, want %+v", got, want)
	}

	returned := machine.Graph()
	returned.Nodes[0].ID = "mutated-return"
	returned.Edges[0].Target = "mutated-return"
	if got := machine.Graph(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Graph() after return mutation = %+v, want %+v", got, want)
	}
}
