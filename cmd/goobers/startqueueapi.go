package main

import (
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startcontrol"
)

func (u *upSession) configureStartQueue() {
	if u.durableTriggers == nil || u.durableTriggers.startControls == nil || u.setup.InteractiveAccess == nil {
		return
	}
	service := &startcontrol.Service{Controls: u.durableTriggers.startControls, Access: u.setup.InteractiveAccess, Scrubber: journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber())}
	service.Stop = u.cancelControlledStart
	u.durableTriggers.startControls.Cancellation = service.ReconcileCancellation
	u.durableTriggers.startControls.Maintenance = u.restartReplayMaintenance()
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithStartQueue(service))
	u.setup.InteractiveAccess.SetQueueCancellationAvailable(true)
}
