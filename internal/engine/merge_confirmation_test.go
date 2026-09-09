package engine

import (
	"encoding/json"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/providers"
)

func TestEngineProjectsSurrenderedMergeConfirmation(t *testing.T) {
	confirmation := providers.MergeConfirmation{RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", MergeSHA: "commit"}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.ExecuteWorkflow(func(ctx workflow.Context) (JournalProjection, error) {
		recorder := &runJournal{}
		recorder.mutations(ctx, "land", 1, journal.AttemptPolicy, surrenderedMutationFacts([]dispatcher.SurrenderedMutation{{
			Provider: "github", Kind: "pr", ID: "9", Operation: "merge", MergeConfirmation: &confirmation,
			ReceiptID: "durable-merge-receipt",
		}}))
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
		t.Fatalf("missing mutation projection: %+v", projection)
	}
	event := projection.Ops[0].Event
	if event.Runner["mutationReceiptId"] != "durable-merge-receipt" {
		t.Fatal("surrendered receipt identity lost during journal projection")
	}
	data, err := json.Marshal(event.Runner["mergeConfirmation"])
	if err != nil {
		t.Fatal(err)
	}
	var got providers.MergeConfirmation
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != confirmation || event.ExternalRef == nil || event.ExternalRef.ID != got.PullID || event.Runner["operation"] != "merge" {
		t.Fatalf("confirmation lost/misattributed: event=%+v confirmation=%+v", event, got)
	}
}

func TestEngineProjectsSurrenderedQueueAdmission(t *testing.T) {
	admission := providers.QueueAdmission{RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", EntryID: "MQE_owned", ExpectedHeadSHA: "head", EnqueuedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.ExecuteWorkflow(func(ctx workflow.Context) (JournalProjection, error) {
		recorder := &runJournal{}
		recorder.mutations(ctx, "land", 1, journal.AttemptPolicy, surrenderedMutationFacts([]dispatcher.SurrenderedMutation{{Provider: "github", Kind: "pr", ID: "9", Operation: "enqueue", QueueAdmission: &admission}}))
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
		t.Fatalf("missing queue admission projection: %+v", projection)
	}
	event := projection.Ops[0].Event
	data, err := json.Marshal(event.Runner["queueAdmission"])
	if err != nil {
		t.Fatal(err)
	}
	var got providers.QueueAdmission
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != admission || event.Runner["operation"] != "enqueue" || event.Runner["mergeConfirmation"] != nil {
		t.Fatalf("queue admission lost/promoted: %+v %+v", got, event)
	}
}
