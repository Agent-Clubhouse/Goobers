package workflow

import (
	"encoding/json"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestGraphProjectsLinearWorkflow(t *testing.T) {
	m, err := Compile(Definition{Name: "linear", Version: 3, Spec: linearSpec()})
	if err != nil {
		t.Fatal(err)
	}

	want := Graph{
		Name:    "linear",
		Version: 3,
		Digest:  m.Digest(),
		Start:   "implement",
		Nodes: []GraphNode{
			{ID: "implement", Kind: GraphNodeAgentic, Owner: "coder"},
		},
		Edges: []GraphEdge{
			{Source: "implement", Terminal: GraphTerminalComplete},
		},
	}
	if got := m.Graph(); !reflect.DeepEqual(got, want) {
		t.Fatalf("graph = %+v, want %+v", got, want)
	}
}

func graphDefinition() Definition {
	return Definition{
		Name:    "delivery",
		Version: 7,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			Start:    "prepare",
			Tasks: []apiv1.Task{
				{
					Name: "prepare", Type: apiv1.TaskDeterministic, Goal: "prepare",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review",
				},
				{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "review"},
				{
					Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}},
				},
			},
			Gates: []apiv1.Gate{
				{
					Name:      "review",
					Evaluator: apiv1.EvaluatorHuman,
					Human:     &apiv1.HumanGate{},
					Branches: map[string]string{
						"needs-changes": "implement",
						"escalate":      TargetEscalate,
						"fail":          TargetAbort,
						"pass":          "finish",
					},
				},
			},
		},
	}
}

func TestGraphProjectionGolden(t *testing.T) {
	m, err := Compile(graphDefinition())
	if err != nil {
		t.Fatal(err)
	}

	got, err := json.MarshalIndent(m.Graph(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const want = `{
  "name": "delivery",
  "version": 7,
  "digest": "sha256:c19e45f5a6073dc6127fa873096434bfd42a4d87658ca5fd9d0b90abcefb38dd",
  "start": "prepare",
  "nodes": [
    {
      "id": "prepare",
      "kind": "deterministic"
    },
    {
      "id": "implement",
      "kind": "agentic",
      "owner": "coder"
    },
    {
      "id": "finish",
      "kind": "deterministic"
    },
    {
      "id": "review",
      "kind": "gate",
      "evaluator": "human"
    }
  ],
  "edges": [
    {
      "source": "prepare",
      "target": "review"
    },
    {
      "source": "implement",
      "target": "review"
    },
    {
      "source": "finish",
      "target": "",
      "terminal": "complete"
    },
    {
      "source": "review",
      "target": "finish",
      "outcome": "pass"
    },
    {
      "source": "review",
      "target": "@abort",
      "outcome": "fail",
      "terminal": "abort"
    },
    {
      "source": "review",
      "target": "@escalate",
      "outcome": "escalate",
      "terminal": "escalate"
    },
    {
      "source": "review",
      "target": "implement",
      "outcome": "needs-changes"
    }
  ]
}`
	if string(got) != want {
		t.Fatalf("graph JSON:\n%s\nwant:\n%s", got, want)
	}
}

func TestGraphSerializationIsDeterministic(t *testing.T) {
	first := graphDefinition()
	second := graphDefinition()
	second.Spec.Gates[0].Branches = map[string]string{}
	for _, outcome := range []string{"fail", "needs-changes", "pass", "escalate"} {
		second.Spec.Gates[0].Branches[outcome] = first.Spec.Gates[0].Branches[outcome]
	}

	m1, err := Compile(first)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Compile(second)
	if err != nil {
		t.Fatal(err)
	}
	got1, err := json.Marshal(m1.Graph())
	if err != nil {
		t.Fatal(err)
	}
	got2, err := json.Marshal(m2.Graph())
	if err != nil {
		t.Fatal(err)
	}
	if string(got1) != string(got2) {
		t.Fatalf("identical definitions serialized differently:\n%s\n%s", got1, got2)
	}
}

func TestGraphProjectsAgenticGateOwner(t *testing.T) {
	m, err := Compile(Definition{Name: "gated", Version: 1, Spec: gatedSpec()})
	if err != nil {
		t.Fatal(err)
	}

	graph := m.Graph()
	for _, node := range graph.Nodes {
		if node.ID == "review" {
			if node.Kind != GraphNodeGate || node.Evaluator != apiv1.EvaluatorAgentic || node.Owner != "reviewer" {
				t.Fatalf("review node = %+v", node)
			}
			return
		}
	}
	t.Fatal("review node not projected")
}
