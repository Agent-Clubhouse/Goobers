package e2e

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// WriteAPIObserver is S7's named observer (goobernetes-smoke.md §4 S7): "the
// write API's journaled trigger-ingestion event carrying the run it
// admitted, and the escalation's resolution event carrying the API-submitted
// decision, followed by the run proceeding. Per D3, the procedure transcript
// itself is an observer: it contains no kubectl exec." (Deliberately not
// quoted verbatim in the constant below — see noexec.go's matchLine doc
// comment for why a string literal never spells out the forbidden phrase in
// this package.)
const WriteAPIObserver = "journal.EventTriggerFired + journal.EventRunResumed (Actor/Action) + procedure transcript scanned for the forbidden pod-exec pattern"

// WriteAPIEvidence is what S7 needs about one run: the trigger-ingestion
// event that admitted it and the escalation-resolution event that unblocked
// it, both read straight off the run's own journal (no separate write-API
// audit surface needed — internal/httpapi's trigger/escalation planes,
// internal/httpapi/writeplanes.go, write through the SAME journal writer
// every local path uses, so the journal IS the write-API's own record).
type WriteAPIEvidence struct {
	// TriggerEvent is the run's journal.EventTriggerFired event.
	TriggerEvent *journal.Event
	// ResolutionEvent is the journal.EventRunResumed event recording the
	// escalation's resolution (Actor/Action carry who resolved it and how —
	// internal/journal/event.go:286-291).
	ResolutionEvent *journal.Event
	// RunProceeded reports whether the run advanced past the escalation
	// (its terminal phase is not journal.PhaseEscalated, or a later stage
	// started after ResolutionEvent's Seq).
	RunProceeded bool
	// ProcedureTranscript is the operator-facing record of every command run
	// during the procedure (§5 rule 1: "No kubectl exec, no hand edits...
	// Either appearing in the transcript is a filed defect").
	ProcedureTranscript string
}

// AssertTriggerAndEscalationViaWriteAPI is S7: the smoke's runs are
// triggered through the write API (never a file drop, never kubectl exec),
// and one run's HITL escalation is resolved through the write API too.
func AssertTriggerAndEscalationViaWriteAPI(evidence WriteAPIEvidence) AssertionResult {
	if evidence.TriggerEvent == nil {
		return invalid("no trigger event supplied", nil)
	}
	if evidence.TriggerEvent.Type != journal.EventTriggerFired {
		return classify("", false,
			fmt.Sprintf("supplied trigger event has Type %q, want %q — the run was not admitted through the trigger plane", evidence.TriggerEvent.Type, journal.EventTriggerFired),
			nil, evidence.TriggerEvent)
	}

	if evidence.ResolutionEvent == nil {
		return invalid("no escalation resolution event supplied", nil)
	}
	if evidence.ResolutionEvent.Type != journal.EventRunResumed {
		return classify("", false,
			fmt.Sprintf("supplied resolution event has Type %q, want %q", evidence.ResolutionEvent.Type, journal.EventRunResumed),
			nil, evidence.ResolutionEvent)
	}
	if strings.TrimSpace(evidence.ResolutionEvent.Actor) == "" {
		return classify("", false, "escalation resolution event carries no Actor — cannot show it was API-submitted by an identified principal", nil, evidence.ResolutionEvent)
	}
	if strings.TrimSpace(evidence.ResolutionEvent.Action) == "" {
		return classify("", false, "escalation resolution event carries no Action", nil, evidence.ResolutionEvent)
	}

	if violations := ScanTextForKubectlExec(evidence.ProcedureTranscript); len(violations) > 0 {
		return classify("", false,
			fmt.Sprintf("procedure transcript contains a forbidden pod-exec command (D3: a filed defect, #3278-style): %v", violations),
			nil, violations)
	}

	if !evidence.RunProceeded {
		return classify("", false, "escalation was resolved but the run never proceeded past it", nil, evidence)
	}

	return classify("", true, "", evidence, nil)
}
