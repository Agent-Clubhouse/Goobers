package vnext

import (
	"encoding/json"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

func TestGraphProjectsLinearWorkflow(t *testing.T) {
	m, err := graphMachine(Definition{Name: "linear", Version: 3, Spec: linearGraphSpec()})
	if err != nil {
		t.Fatal(err)
	}

	want := model.Graph{
		Name:    "linear",
		Version: 3,
		Digest:  m.Digest(),
		Start:   "implement",
		Nodes: []model.GraphNode{
			{ID: "implement", Kind: model.GraphNodeAgentic, Owner: "coder"},
		},
		Edges: []model.GraphEdge{
			{Source: "implement", Terminal: model.GraphTerminalComplete},
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
					// #706: human gates are rejected at compile time until
					// durable pause/resume ships — this fixture only needs a
					// multi-branch gate to exercise graph projection, not
					// human-gate semantics specifically, so it uses the same
					// agentic-gate shape the compiler tests already
					// does. escalate's terminal-edge projection shape (the
					// same TargetEscalate handling graphTerminal switches on)
					// no longer has a dedicated case here since agentic gates
					// only produce pass/fail/needs-changes outcomes.
					Name:      "review",
					Evaluator: apiv1.EvaluatorAgentic,
					Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
					Branches: map[string]string{
						"needs-changes": "implement",
						"fail":          TargetAbort,
						"pass":          "finish",
					},
				},
			},
		},
	}
}

func TestGraphProjectionGolden(t *testing.T) {
	m, err := graphMachine(graphDefinition())
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
  "digest": "sha256:5a33502988ae8377f3e4ca7cea1b6cf863dd10709c4affaaf52058a455570ef2",
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
      "owner": "reviewer",
      "evaluator": "agentic"
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
	for _, outcome := range []string{"fail", "needs-changes", "pass"} {
		second.Spec.Gates[0].Branches[outcome] = first.Spec.Gates[0].Branches[outcome]
	}

	m1, err := graphMachine(first)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := graphMachine(second)
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
	m, err := graphMachine(Definition{Name: "gated", Version: 1, Spec: gatedGraphSpec()})
	if err != nil {
		t.Fatal(err)
	}

	graph := m.Graph()
	for _, node := range graph.Nodes {
		if node.ID == "review" {
			if node.Kind != model.GraphNodeGate || node.Evaluator != apiv1.EvaluatorAgentic || node.Owner != "reviewer" {
				t.Fatalf("review node = %+v", node)
			}
			return
		}
	}
	t.Fatal("review node not projected")
}

func graphMachine(def Definition) (*Machine, error) {
	return newMachine(def)
}

func linearGraphSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"},
		},
	}
}

func gatedGraphSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          TerminalComplete,
					"fail":          TargetAbort,
					"needs-changes": "implement",
				},
			},
		},
	}
}
