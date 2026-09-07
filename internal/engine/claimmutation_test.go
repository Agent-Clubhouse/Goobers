package engine

import (
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestSurrenderedClaimOutcomeSurvivesConversion(t *testing.T) {
	facts := surrenderedMutationFacts([]dispatcher.SurrenderedMutation{{
		Provider: "github", Kind: "issue", ID: "7", Operation: "claim",
		RunID: "lease-owner", Outcome: "conflict", ErrorCode: "provider_claim_conflict", ProviderRunID: "provider-owner",
	}})
	if len(facts) != 1 || facts[0].RunID != "lease-owner" || facts[0].Outcome != "conflict" || facts[0].ErrorCode != "provider_claim_conflict" || facts[0].ProviderRunID != "provider-owner" {
		t.Fatalf("pod outcome dropped: %+v", facts)
	}
}

func TestEngineClaimOutcomesAreStructuredJournalErrors(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.ExecuteWorkflow(func(ctx workflow.Context) (JournalProjection, error) {
		recorder := &runJournal{}
		for _, outcome := range []string{"success", "conflict", "failure"} {
			recorder.mutations(ctx, "reconcile", 2, journal.AttemptPolicy,
				surrenderedMutationFacts([]dispatcher.SurrenderedMutation{{
					Provider: "github", Kind: "issue", ID: "7", Operation: "claim",
					RunID: "lease-owner", Outcome: outcome, ProviderRunID: "provider-owner",
				}}))
		}
		return recorder.proj, nil
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var projection JournalProjection
	if err := env.GetWorkflowResult(&projection); err != nil {
		t.Fatal(err)
	}
	if len(projection.Ops) != 3 {
		t.Fatalf("outcomes lost: %+v", projection)
	}
	for i, operation := range projection.Ops {
		event := operation.Event
		if event == nil || event.Runner["providerRunId"] != "provider-owner" {
			t.Fatalf("provider owner lost: %+v", event)
		}
		if event == nil || event.ExternalRef == nil || event.ExternalRef.ID != "7" || event.Stage != "reconcile" || event.Attempt != 2 || event.AttemptClass != journal.AttemptPolicy || event.Runner["claimRunId"] != "lease-owner" {
			t.Fatalf("claim attribution lost: %+v", event)
		}
		if i == 0 {
			if event.Type != journal.EventRefTouched || event.Error != nil {
				t.Fatalf("successful mutation misclassified: %+v", event)
			}
		} else if event.Type != journal.EventError || event.Error == nil || event.Error.Code == "" {
			t.Fatalf("unsuccessful attempt counted as a mutation: %+v", event)
		}
	}
}
