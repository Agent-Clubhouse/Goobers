package main

import (
	"context"

	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// The caller holds current queue.cancel authority. Source adapters must not
// reenter that policy lock or infer termination from cancellation delivery.
func (s *durableTriggerService) cancelTypedQueueStart(ctx context.Context, c triggerqueue.StartControl) (startcontrol.CancellationObservation, bool, error) {
	if c.Scope.Source == "session" && s.sessions != nil {
		observation, err := s.sessions.CancelQueuedTurn(ctx, c.Scope.Gaggle, c.Record.ID)
		return observation, true, err
	}
	if c.Scope.Source == "child" {
		observation, err := s.cancelChildQueueStart(ctx, c)
		return observation, true, err
	}
	if c.Scope.Source == "session" {
		return startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}, true, nil
	}
	return startcontrol.CancellationObservation{}, false, nil
}
