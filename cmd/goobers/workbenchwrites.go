package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func (u *upSession) installWorkbenchWrites(read *workbenchservice.Service) {
	available := u.durableTriggers != nil && u.durableTriggers.queue != nil && read.Backlog != nil
	u.setup.InteractiveAccess.SetBacklogEditAvailable(available)
	u.installWorkbenchResolution(read, available)
	if !available {
		return
	}
	writer := &workbenchservice.WriterService{ReadService: read, Queue: u.durableTriggers.queue}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithWorkbenchWrites(writer))
	if u.setup.SessionBacklogWriter == nil && u.credentialPlane != nil && u.credentialPlane.grants != nil {
		u.setup.SessionBacklogWriter = workbenchSessionWriter(writer)
	}

}

// Terminal editing receipts share the daemon's existing bounded maintenance pass.
// Attempting/unknown commands remain held; pruning never contacts the provider.
func (s *durableTriggerService) pruneWorkbenchCommands(ctx context.Context) error {
	scope, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err := s.queue.PruneWorkbenchCommands(scope, s.dispatch.now(), 100)
	return err
}

func (u *upSession) installWorkbenchResolution(read *workbenchservice.Service, writesAvailable bool) {
	available := writesAvailable && u.credentialPlane != nil && u.credentialPlane.grants != nil && filepath.IsAbs(u.l.SchedulerDir())
	u.setup.InteractiveAccess.SetBacklogResolveAvailable(available)
	if !available || u.setup.SessionBacklogResolver != nil {
		return
	}
	resolver := &workbenchservice.ResolutionService{ReadService: read, Queue: u.durableTriggers.queue, WithLearnedBlock: workbenchLearnedBlocks(u.l.SchedulerDir())}
	u.setup.SessionBacklogResolver = workbenchSessionResolver(resolver)
}
