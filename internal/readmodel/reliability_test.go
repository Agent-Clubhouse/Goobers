package readmodel

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func foldReliability(events ...journal.Event) ReliabilityFacts {
	var facts ReliabilityFacts
	for _, event := range events {
		event.Schema = journal.EventSchema
		facts = facts.After(event)
	}
	return facts
}

func TestReliabilityFactsFoldTerminalCauseDraftAndAcceptance(t *testing.T) {
	cause := &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: journal.PhaseEscalated, Code: "review-escalated",
		Repass: &journal.TerminalBudget{Consumed: 2, Allowed: 3}}
	finished := journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated), TerminalCause: cause}

	facts := foldReliability(
		journal.Event{Type: journal.EventArtifactRecorded, Name: AcceptanceMappingArtifact, Ref: &journal.Ref{Digest: "sha256:map"}},
		// JSON round-trips a numeric child-publication prNumber as float64.
		journal.Event{Type: journal.EventStageFinished, Outputs: map[string]any{"prNumber": float64(7), "draft": true}},
		finished,
	)
	if facts.AcceptanceState != AcceptanceStateRecorded || facts.AcceptanceDigest != "sha256:map" {
		t.Fatalf("artifact-only acceptance = %q/%q", facts.AcceptanceState, facts.AcceptanceDigest)
	}
	if facts.PullRequestDraft == nil || *facts.PullRequestDraft != (PullRequestDraft{ID: "7", Draft: true}) {
		t.Fatalf("draft = %+v", facts.PullRequestDraft)
	}
	if facts.TerminalCause == nil || facts.TerminalCause.Code != "review-escalated" || facts.TerminalCause.Repass == cause.Repass {
		t.Fatalf("terminal cause must be a deep copy: %+v", facts.TerminalCause)
	}

	resumed := foldReliability(finished, journal.Event{Type: journal.EventRunResumed, Target: "implement"})
	if resumed.TerminalCause != nil {
		t.Fatalf("resume must clear the previous generation's cause: %+v", resumed.TerminalCause)
	}
	legacy := foldReliability(finished, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)})
	if legacy.TerminalCause != nil {
		t.Fatalf("a later run.finished without a record must not keep an older cause: %+v", legacy.TerminalCause)
	}

	partial := foldReliability(journal.Event{Type: journal.EventStageFinished, Outputs: map[string]any{"prNumber": "9", "draft": "maybe"}})
	if partial.PullRequestDraft != nil {
		t.Fatalf("unparseable draft output must stay unknown: %+v", partial.PullRequestDraft)
	}
}
