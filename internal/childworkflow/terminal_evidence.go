package childworkflow

import (
	"errors"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// MaxChildResultArtifactBytes bounds one artifact a child's result references.
const MaxChildResultArtifactBytes = 1 << 20

// TerminalInputFromJournal derives a terminal child's bounded result input
// from its journal: the state from its terminal phase, the finish time from
// its run.finished event, and the result evidence (ResultEvidence). found is
// false while the child is not terminal. Shared by the local terminal
// observer and the control plane's engine-child finalizer.
func TerminalInputFromJournal(rd journal.RunReader, events []journal.Event, phase journal.RunPhase) (TerminalResultInput, bool, error) {
	var state triggerqueue.ChildState
	switch phase {
	case journal.PhaseCompleted:
		state = triggerqueue.ChildCompleted
	case journal.PhaseFailed:
		state = triggerqueue.ChildFailed
	case journal.PhaseAborted:
		state = triggerqueue.ChildCancelled
	default:
		return TerminalResultInput{}, false, nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != journal.EventRunFinished {
			continue
		}
		if events[i].Status != string(phase) || events[i].Time.IsZero() {
			return TerminalResultInput{}, false, errors.New("child terminal event disagrees with phase")
		}
		input := TerminalResultInput{State: state, FinishedAt: events[i].Time, Summary: "Child workflow " + string(phase)}
		refs, summary, err := ResultEvidence(rd, events[:i+1])
		if err != nil {
			return TerminalResultInput{}, false, err
		}
		input.References = refs
		if summary != "" {
			input.Summary = summary
		}
		return input, true, nil
	}
	return TerminalResultInput{}, false, nil
}

// ResultEvidence returns at most 16 distinct readable artifact references,
// newest event first, and the latest stage summary bounded to 8 KiB on a
// UTF-8 boundary. An unreadable referenced artifact is an error, never
// dropped.
func ResultEvidence(rd journal.RunReader, events []journal.Event) ([]string, string, error) {
	seen := map[string]bool{}
	var refs []string
	var summary string
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if summary == "" && event.Type == journal.EventStageFinished {
			if text, ok := event.Outputs["summary"].(string); ok && utf8.ValidString(text) {
				summary = boundedChildSummary(text)
			}
		}
		for _, ref := range event.Artifacts {
			if len(refs) == 16 || seen[ref.Digest] || ref.Size > MaxChildResultArtifactBytes {
				continue
			}
			if _, err := rd.ArtifactBytesBounded(ref, MaxChildResultArtifactBytes); err != nil {
				return nil, "", err
			}
			seen[ref.Digest] = true
			refs = append(refs, ref.Digest)
		}
	}
	return refs, summary, nil
}

func boundedChildSummary(text string) string {
	if len(text) <= 8192 {
		return text
	}
	text = text[:8192]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
