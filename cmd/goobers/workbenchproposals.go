package main

import (
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func (u *upSession) installWorkbenchProposals(read *workbenchservice.Service, provider workbenchservice.RepositoryProposalFactory) {
	available := read != nil && read.Permissions != nil && read.Repository != nil && provider != nil && u.durableTriggers != nil && u.durableTriggers.queue != nil
	u.setup.InteractiveAccess.SetSourceProposalAvailable(available)
	if !available {
		return
	}
	service := &workbenchservice.ProposalService{ReadService: read, Queue: u.durableTriggers.queue, Provider: provider, Now: u.durableTriggers.dispatch.now}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithWorkbenchProposals(service))
	u.installWorkbenchSuggestions(service)
}
