package runner

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// ReconcileChildWorkspaceWriter closes only the repository writer nested
// between one original stage start and its host-owned pod dispatch marker.
// The caller must already hold exclusive execution custody and have verified
// exact worker termination and imported that worker's retained result. This
// method cannot derive process termination from journal lifecycle events.
func ReconcileChildWorkspaceWriter(reader *journal.Reader, writer *journal.Run, id journal.RunIdentity, stageStart, podStart journal.Event) error {
	admission, err := PinnedChildWorkspaceAdmission(reader, id)
	if err != nil {
		return err
	}
	if admission == nil {
		return nil
	}
	if err := verifyChildWriterPolicy(reader, id); err != nil {
		return err
	}
	if writer == nil || writer.Dir() != reader.Dir() || stageStart.Seq == 0 || podStart.Seq <= stageStart.Seq || stageStart.Branch != 0 || podStart.Branch != 0 || stageStart.Stage != podStart.Stage || stageStart.Attempt != podStart.Attempt {
		return invoke.ErrWorkspaceNotQuiescent
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	candidate, joined, err := childRepositoryWriterScope(events, id, stageStart, podStart)
	if err != nil {
		return err
	}

	if candidate == nil {
		return fmt.Errorf("%w: original repository writer missing", invoke.ErrWorkspaceNotQuiescent)
	}
	scope, _ := candidate.Runner["writerScope"].(string)
	if event, ok := joined[scope]; ok {
		if event.Seq <= candidate.Seq || event.Stage != candidate.Stage || event.Attempt != candidate.Attempt || event.Branch != candidate.Branch {
			return invoke.ErrWorkspaceNotQuiescent
		}
		return VerifyChildWorkspaceQuiescence(reader, id, events)
	}
	return writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: candidate.Stage, Attempt: candidate.Attempt, Branch: candidate.Branch, Runner: map[string]any{"kind": childWriterJoined, "writerScope": scope}})
}

func childRepositoryWriterScope(events []journal.Event, id journal.RunIdentity, stageStart, podStart journal.Event) (*journal.Event, map[string]journal.Event, error) {
	var candidate *journal.Event
	joined := map[string]journal.Event{}
	for i := range events {
		event := events[i]
		kind, _ := event.Runner["kind"].(string)
		if event.Type != journal.EventRunnerAnnotation || (kind != childWriterStarted && kind != childWriterJoined) {
			continue
		}
		scope, ok := event.Runner["writerScope"].(string)
		if !ok || len(scope) != 32 || !apiv1.ValidRunID(scope) {
			return nil, nil, invoke.ErrWorkspaceNotQuiescent
		}
		if kind == childWriterJoined {
			joined[scope] = event
			continue
		}
		if event.Seq <= stageStart.Seq || event.Seq >= podStart.Seq {
			continue
		}
		if event.Stage != id.RunID+":"+stageStart.Stage || event.Attempt != stageStart.Attempt || event.Branch != 0 || candidate != nil {
			return nil, nil, invoke.ErrWorkspaceNotQuiescent
		}
		candidate = &event
	}
	return candidate, joined, nil
}
