package main

import (
	"context"

	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Typed adapters replace this conservative placeholder once their existing
// source owners can request and observe cancellation of this exact invocation.
func (s *durableTriggerService) cancelTypedQueueStart(_ context.Context, c triggerqueue.StartControl) (startcontrol.CancellationObservation, bool, error) {
	if c.Scope.Source == "child" || c.Scope.Source == "session" {
		return startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}, true, nil
	}
	return startcontrol.CancellationObservation{}, false, nil
}
