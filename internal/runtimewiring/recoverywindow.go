package runtimewiring

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// RecoveryCaptureWindow determines the stable capture identity and terminal state.
// Terminal snapshots use a new, stable identity anchored to the durable finish
// event. This gives long-running jobs a recovery window without rewriting the
// immutable metadata of an earlier stage snapshot or extending it on retries.
//
// Whether a capture is terminal is a property of the owning run, read from its
// journal, never of the caller that installed the cleanup guard: one daemon
// process finalizes many runs, and a decision carried on the callback would
// answer for the run that installed it rather than the run being cleaned up.
func RecoveryCaptureWindow(ctx context.Context, reader *journal.Reader, startedAt time.Time) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	events, err := reader.Events()
	if err != nil {
		return time.Time{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	captureAt, err := RecoveryWindowTime(events, startedAt)
	if err != nil {
		return time.Time{}, false, err
	}
	return captureAt, journal.PhaseFromEvents(events) != journal.PhaseRunning, nil
}

// RecoveryCaptureTime is recoveryCaptureWindow for callers that already know
// the run reached a terminal phase and need only the capture anchor.
func RecoveryCaptureTime(ctx context.Context, reader *journal.Reader, startedAt time.Time) (time.Time, error) {
	captureAt, _, err := RecoveryCaptureWindow(ctx, reader, startedAt)
	return captureAt, err
}

// RecoveryWindowTime selects the timestamp used for a recovery retention window.
func RecoveryWindowTime(events []journal.Event, startedAt time.Time) (time.Time, error) {
	if journal.PhaseFromEvents(events) == journal.PhaseRunning {
		return startedAt, nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Type {
		case journal.EventRunResumed, journal.EventStageRerunRequested, journal.EventGateOverridden:
			return time.Time{}, fmt.Errorf("terminal recovery requires a finish event for the current execution")
		case journal.EventRunFinished:
			if event.Time.IsZero() || event.Time.Before(startedAt) {
				return time.Time{}, fmt.Errorf("terminal recovery requires a valid durable finish timestamp")
			}
			return event.Time, nil
		}
	}
	return time.Time{}, fmt.Errorf("terminal recovery is waiting for its durable finish event")
}
