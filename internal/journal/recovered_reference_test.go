package journal

import "testing"

func TestRecoveredReferencePreservesNormalOutcomeSemantics(t *testing.T) {
	ref := &ExternalRef{Provider: "github", Kind: "pr", ID: "42"}
	for _, outcome := range []string{"", "success", "failure", "conflict"} {
		normal := WithMutationOutcome(Event{Type: EventRefTouched, ExternalRef: ref}, "", outcome, "", "")
		recovered := normal
		recovered.Type = EventRunnerMutationRecovered
		want := outcome != "failure" && outcome != "conflict"
		if normal.IsReferenceTouch() != want || recovered.IsReferenceTouch() != want {
			t.Fatalf("outcome %q: normal=%v recovered=%v", outcome, normal.IsReferenceTouch(), recovered.IsReferenceTouch())
		}
		// Even a malformed recovered failure without its error detail must
		// not become a successful reference merely because it has an ID.
		recovered.Error = nil
		if recovered.IsReferenceTouch() != want {
			t.Fatalf("outcome %q depended on error detail", outcome)
		}
	}
	for _, event := range []Event{
		{Type: EventRunnerMutationRecovered},
		{Type: EventRunnerMutationRecovered, ExternalRef: ref, Error: &ErrorDetail{Code: "unknown"}},
		{Type: EventError, ExternalRef: ref},
		{Type: EventRunnerAnnotation, ExternalRef: ref},
	} {
		if event.IsReferenceTouch() {
			t.Fatalf("non-touch event admitted: %+v", event)
		}
	}
}
