package engine

import (
	"encoding/json"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mutationreceipt"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestEngineProjectsSemanticMutationMetadata(t *testing.T) {
	identity, _ := mutationreceipt.New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", "body")
	for _, phase := range []string{"intent", "completed"} {
		t.Run(phase, func(t *testing.T) {
			receipt := mutationreceipt.Receipt{Schema: 1, ID: "invocation", RunID: "source", Mutation: identity, Phase: phase}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.ExecuteWorkflow(func(ctx workflow.Context) (JournalProjection, error) {
				recorder := &runJournal{}
				recorder.mutations(ctx, "post", 2, journal.AttemptInfra, surrenderedMutationFacts([]dispatcher.SurrenderedMutation{{Provider: "github", Kind: "issue", ID: "7", ReceiptID: "custody", Operation: "comment", SemanticMutation: &receipt}}))
				return recorder.proj, nil
			})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var projection JournalProjection
			if err := env.GetWorkflowResult(&projection); err != nil {
				t.Fatal(err)
			}
			if len(projection.Ops) != 1 || projection.Ops[0].Event == nil {
				t.Fatalf("missing projection: %#v", projection)
			}
			event := projection.Ops[0].Event
			raw, err := json.Marshal(event.Runner["semanticMutation"])
			if err != nil {
				t.Fatal(err)
			}
			var got mutationreceipt.Receipt
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got != receipt || event.Stage != "post" || event.Attempt != 2 || event.IsReferenceTouch() {
				t.Fatalf("capture metadata lost or counted as another effect: %#v", event)
			}
		})
	}
}
