package runner

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mutationreceipt"
)

type semanticJournal struct {
	executionJournal
	events []journal.Event
}

func (j *semanticJournal) Append(event journal.Event) error {
	j.events = append(j.events, event)
	return nil
}

func TestRunnerProjectsSemanticMutationMetadata(t *testing.T) {
	identity, _ := mutationreceipt.New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", "body")
	for _, phase := range []string{"intent", "completed"} {
		t.Run(phase, func(t *testing.T) {
			receipt := mutationreceipt.Receipt{Schema: 1, ID: "invocation", RunID: "source", Mutation: identity, Phase: phase}
			// Model the JSON boundary from the separate stage process.
			raw, err := json.Marshal(mutationFact{ReceiptID: "custody", Provider: "github", Kind: "issue", ID: "7", Operation: "comment", SemanticMutation: &receipt})
			if err != nil {
				t.Fatal(err)
			}
			var fact mutationFact
			if err := json.Unmarshal(raw, &fact); err != nil {
				t.Fatal(err)
			}
			recorder := &semanticJournal{}
			done := make(chan error)
			close(done)
			if err := finishTaskDispatch(recorder, stageHeartbeat{stop: make(chan struct{}), done: done}, "post", 2, journal.AttemptInfra, []mutationFact{fact}, nil); err != nil {
				t.Fatal(err)
			}
			if len(recorder.events) != 1 {
				t.Fatalf("events=%#v", recorder.events)
			}
			event := recorder.events[0]
			got, ok := event.Runner["semanticMutation"].(mutationreceipt.Receipt)
			if !ok || got != receipt || event.Stage != "post" || event.Attempt != 2 || event.IsReferenceTouch() {
				t.Fatalf("capture metadata lost or counted as another effect: %#v", event)
			}
		})
	}
}
