package journal

import "testing"

func TestMutationOutcomeDoesNotCountFailuresAsTouches(t *testing.T) {
	for _, outcome := range []string{"", "success", "failure", "conflict", "contention"} {
		t.Run(outcome, func(t *testing.T) {
			event := WithMutationOutcome(Event{
				Type: EventRefTouched, RunID: "reconciliation-run", Runner: map[string]any{"operation": "claim"},
				ExternalRef: &ExternalRef{Provider: "github", Kind: "issue", ID: "7"},
			}, "lease-owner", outcome, "", "provider-owner")
			if event.RunID != "reconciliation-run" || event.Runner["claimRunId"] != "lease-owner" {
				t.Fatalf("run attribution changed: %+v", event)
			}
			failed := outcome == "failure" || outcome == "conflict"
			if outcome == "contention" {
				if event.Type != EventRunnerAnnotation || event.Error != nil || event.ExternalRef != nil ||
					event.Runner["annotation"] != "provider-claim-contention" || event.Runner["providerRunId"] != "provider-owner" ||
					event.Runner["provider"] != "github" || event.Runner["kind"] != "issue" || event.Runner["itemId"] != "7" {
					t.Fatalf("contention lacks actionable owner identity: %+v", event)
				}
			}
			if (event.Type == EventError) != failed || (event.Error != nil) != failed {
				t.Fatalf("outcome %s misclassified: %+v", outcome, event)
			}
			if event.Runner["operation"] != "claim" {
				t.Fatal("lost operation")
			}
			if failed && event.Error.Code != "provider_claim_"+outcome {
				t.Fatalf("incorrect fallback classification: %+v", event.Error)
			}
		})
	}
}
