package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Terminal snapshots use a new, stable identity anchored to the durable finish
// event. This gives long-running jobs a recovery window without rewriting the
// immutable metadata of an earlier stage snapshot or extending it on retries.
func recoveryCaptureTime(ctx context.Context, reader *journal.Reader, startedAt time.Time, terminal bool) (time.Time, error) {
	if !terminal {
		return startedAt, nil
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	events, err := reader.Events()
	if err != nil {
		return time.Time{}, err
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	return recoveryWindowTime(events, startedAt)
}

func recoveryWindowTime(events []journal.Event, startedAt time.Time) (time.Time, error) {
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
