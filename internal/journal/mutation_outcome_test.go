package journal

import "testing"

func TestMutationOutcomeDoesNotCountFailuresAsTouches(t *testing.T) {
	for _, outcome := range []string{"", "success", "failure", "conflict"} {
		t.Run(outcome, func(t *testing.T) {
			event := WithMutationOutcome(Event{Type: EventRefTouched, RunID: "reconciliation-run", Runner: map[string]any{"operation": "claim"}}, "lease-owner", outcome, "", "")
			if event.RunID != "reconciliation-run" || event.Runner["claimRunId"] != "lease-owner" {
				t.Fatalf("run attribution changed: %+v", event)
			}
			failed := outcome == "failure" || outcome == "conflict"
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
