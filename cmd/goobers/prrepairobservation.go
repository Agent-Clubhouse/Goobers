package main

import (
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func (u *upSession) installPRRepairRecovery(factory workbenchservice.PRRepairFactory) {
	if u.setup == nil || u.setup.InteractiveAccess == nil || u.durableTriggers == nil || u.durableTriggers.queue == nil || factory == nil {
		return
	}
	service := &workbenchservice.PRRepairRecoveryService{Queue: u.durableTriggers.queue, Permissions: u.setup.InteractiveAccess, Client: factory}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithPRRepairRecovery(service))
}
