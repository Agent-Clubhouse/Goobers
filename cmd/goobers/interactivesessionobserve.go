package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactivesession"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (r *daemonSessionRuntime) observe(ctx context.Context, t triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs) (interactivesession.Observation, error) {
	layout := instance.NewLayout(r.setup.Root)
	directory, err := acceptedTriggerJournalDir(ctx, layout, strings.TrimPrefix(t.Record.ID, "trigger-"))
	if err != nil {
		return interactivesession.Observation{}, err
	}
	if directory == "" {
		return interactivesession.Observation{Absent: true}, nil
	}
	reader, err := journal.OpenReadOnly(directory)
	if err != nil {
		return interactivesession.Observation{}, err
	}
	id, err := reader.Identity()
	if err != nil {
		return interactivesession.Observation{}, err
	}
	owner, err := layout.ReadIdentity()
	if err != nil || id.InstanceID != owner {
		return interactivesession.Observation{}, errors.New("interactive session journal instance mismatch")
	}
	if err = verifySessionJournalInputs(reader, id, inputs); err != nil {
		return interactivesession.Observation{}, err
	}
	events, err := reader.EventsBounded(8<<20, 8192)
	if err != nil {
		return interactivesession.Observation{}, err
	}
	ref, joined, err := journal.SessionWriterEvidence(events, id)
	if err != nil {
		return interactivesession.Observation{}, err
	}
	phase := journal.PhaseFromEvents(events)
	outcome, text := sessionPhaseOutcome(phase)
	if ref != nil {
		raw, readErr := reader.ArtifactBytesBounded(*ref, sessioning.MaxTextBytes)
		if readErr != nil {
			return interactivesession.Observation{}, readErr
		}
		if len(raw) > 0 {
			text = string(raw)
		}
	}
	return interactivesession.Observation{Found: true, Identity: id, Terminal: phase != journal.PhaseRunning, TerminalAt: sessionTerminalTime(events, phase), WritersJoined: joined && sessionRuntimeWasClosed(events, id), Outcome: outcome, Text: text}, ctx.Err()
}

func sessionTerminalTime(events []journal.Event, phase journal.RunPhase) time.Time {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunFinished {
			if events[i].Status == string(phase) && events[i].Seq > 0 {
				return events[i].Time
			}
			break
		}
	}
	return time.Time{}
}
func verifySessionJournalInputs(reader *journal.Reader, id journal.RunIdentity, inputs sessioning.ExecutionInputs) error {
	if id.Session == nil || id.ValidateSessionLineage() != nil {
		return errors.New("interactive session journal lineage is missing")
	}
	expected, err := inputs.Validate(id.RunID, id.Gaggle)
	if err != nil {
		return err
	}
	found := false
	for _, input := range id.Inputs {
		if input.Name != sessioning.ContextInputName {
			continue
		}
		if found || input.Ref.Digest != id.Session.InputDigest {
			return errors.New("interactive session input reference mismatch")
		}
		found = true
		raw, err := reader.ArtifactBytesBounded(input.Ref, sessioning.MaxContextBytes)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, expected) {
			return errors.New("interactive session input content mismatch")
		}
	}
	if !found {
		return errors.New("interactive session journal input missing")
	}
	actual, err := runner.PinnedWorkflowMachine(reader, id)
	if err != nil {
		return err
	}
	expectedMachine, err := sessionMachine(id.Gaggle, inputs.Start.Goober)
	if err != nil || actual.Digest() != expectedMachine.Digest() {
		return errors.New("interactive session machine mismatch")
	}
	return nil
}
func sessionPhaseOutcome(phase journal.RunPhase) (string, string) {
	switch phase {
	case journal.PhaseCompleted:
		return "success", "The turn completed without a response summary."
	case journal.PhaseAborted:
		return "cancelled", "The turn was cancelled."
	case journal.PhaseEscalated:
		return "needs-human", "The turn needs human attention."
	default:
		return "failed", "The turn did not complete. Its run contains the execution details."
	}
}
func sessionResponseText(summary string, runErr error) string {
	summary = strings.TrimSpace(strings.ToValidUTF8(summary, "�"))
	if summary == "" {
		if runErr != nil {
			return "The turn did not complete. Its run contains the execution details."
		}
		return "The turn completed without a response summary."
	}
	if len(summary) > sessioning.MaxTextBytes {
		summary = summary[:sessioning.MaxTextBytes-len("\n[Response truncated]")]
		for !utf8.ValidString(summary) {
			summary = summary[:len(summary)-1]
		}
		summary += "\n[Response truncated]"
	}
	return summary
}
