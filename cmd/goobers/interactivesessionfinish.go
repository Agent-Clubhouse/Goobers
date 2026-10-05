package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/goobers/goobers/internal/journal"
)

const sessionRuntimeClosed = "session.runtime.closed"

// finishSessionRuntime runs only after Start returns and the shared runtime
// proves all processes joined and its isolated credential home was removed.
// A crash before this acknowledgement keeps the turn in custody.
func (e *interactiveRestartExecution) finishSessionRuntime(ctx context.Context) error {
	dir := filepath.Join(e.layout.RunsDir(), e.source.RunID)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	writer, _, err := journal.TryRecover(dir, journal.WithScrubber(journal.Chain(e.setup.SharedRegistry, journal.NewPatternScrubber())))
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil || !reflect.DeepEqual(id.Session, e.source.Session) || id.InstanceID != e.source.InstanceID {
		return errors.New("interactive session cleanup identity mismatch")
	}
	events, err := reader.EventsBounded(8<<20, 8192)
	if err != nil {
		return err
	}
	_, joined, err := journal.SessionWriterEvidence(events, id)
	if err != nil || !joined {
		return errors.Join(errors.New("interactive session cleanup has unjoined writers"), err)
	}
	if journal.PhaseFromEvents(events) == journal.PhaseRunning {
		phase := journal.PhaseFailed
		if ctx.Err() != nil {
			phase = journal.PhaseAborted
		}
		if err = writer.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase), Reason: "interactive turn returned before terminal state"}); err != nil {
			return err
		}
	}
	return writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": sessionRuntimeClosed, "inputDigest": id.Session.InputDigest}})
}
func sessionRuntimeWasClosed(events []journal.Event, id journal.RunIdentity) bool {
	closed := false
	for _, event := range events {
		if event.Type == journal.EventStageStarted || event.Type == journal.EventRunResumed || event.Runner["kind"] == journal.SessionWriterStarted || event.Runner["kind"] == journal.SessionWriterJoined {
			closed = false
		}
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == sessionRuntimeClosed {
			if len(event.Runner) != 2 || event.Runner["inputDigest"] != id.Session.InputDigest {
				return false
			}
			closed = true
		}
	}
	return closed
}
