package goobernetes

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestAssertTriggerAndEscalationViaWriteAPIPass(t *testing.T) {
	trigger := &journal.Event{Type: journal.EventTriggerFired}
	resolution := &journal.Event{Type: journal.EventRunResumed, Actor: "operator@example.com", Action: "approve"}
	evidence := WriteAPIEvidence{
		TriggerEvent: trigger, ResolutionEvent: resolution, RunProceeded: true,
		ProcedureTranscript: "goobers run smoke-workflow --gaggle demo\ngoobers escalation resolve --run r1 --resolution approve\n",
	}
	got := AssertTriggerAndEscalationViaWriteAPI(evidence)
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

func TestAssertTriggerAndEscalationViaWriteAPIFailsWrongTriggerType(t *testing.T) {
	trigger := &journal.Event{Type: journal.EventStageStarted}
	resolution := &journal.Event{Type: journal.EventRunResumed, Actor: "op", Action: "approve"}
	got := AssertTriggerAndEscalationViaWriteAPI(WriteAPIEvidence{TriggerEvent: trigger, ResolutionEvent: resolution, RunProceeded: true})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (wrong trigger event type)", got.Verdict)
	}
}

func TestAssertTriggerAndEscalationViaWriteAPIFailsNoActor(t *testing.T) {
	trigger := &journal.Event{Type: journal.EventTriggerFired}
	resolution := &journal.Event{Type: journal.EventRunResumed, Action: "approve"}
	got := AssertTriggerAndEscalationViaWriteAPI(WriteAPIEvidence{TriggerEvent: trigger, ResolutionEvent: resolution, RunProceeded: true})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (no actor)", got.Verdict)
	}
}

// TestAssertTriggerAndEscalationViaWriteAPICatchesKubectlExec is D3's rule
// applied to the procedure transcript: any kubectl exec anywhere is a
// defect.
func TestAssertTriggerAndEscalationViaWriteAPICatchesKubectlExec(t *testing.T) {
	trigger := &journal.Event{Type: journal.EventTriggerFired}
	resolution := &journal.Event{Type: journal.EventRunResumed, Actor: "op", Action: "approve"}
	transcript := "goobers run smoke-workflow\n" + "kubectl " + "exec -it daemon-pod -- sh\n"
	got := AssertTriggerAndEscalationViaWriteAPI(WriteAPIEvidence{
		TriggerEvent: trigger, ResolutionEvent: resolution, RunProceeded: true, ProcedureTranscript: transcript,
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (transcript contains the forbidden pattern)", got.Verdict)
	}
}

func TestAssertTriggerAndEscalationViaWriteAPIFailsWhenRunNeverProceeded(t *testing.T) {
	trigger := &journal.Event{Type: journal.EventTriggerFired}
	resolution := &journal.Event{Type: journal.EventRunResumed, Actor: "op", Action: "approve"}
	got := AssertTriggerAndEscalationViaWriteAPI(WriteAPIEvidence{TriggerEvent: trigger, ResolutionEvent: resolution, RunProceeded: false})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (run never proceeded)", got.Verdict)
	}
}

func TestAssertTriggerAndEscalationViaWriteAPIInvalidWithoutEvents(t *testing.T) {
	if got := AssertTriggerAndEscalationViaWriteAPI(WriteAPIEvidence{}); got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid", got.Verdict)
	}
}
