package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func (u *upSession) configureWorkbenchReads() {
	factory := workbenchservice.ProviderFactory{SchedulerDirectory: u.l.SchedulerDir(), Registrar: u.setup.SharedRegistry}
	read := &workbenchservice.Service{Permissions: u.setup.InteractiveAccess, Backlog: factory.Backlog, Repository: factory.Repository}
	u.installWorkbenchReads(read)
	if u.setup.SharedRegistry != nil {
		selection := &workbenchservice.PRSelectionService{Permissions: read.Permissions, Client: factory.PRRepair, Scrubber: journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber())}
		u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithPRSelection(selection))
	}
	u.installWorkbenchProposals(read, factory.RepositoryProposal)
}

func (u *upSession) installWorkbenchReads(service *workbenchservice.Service) {
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithWorkbenchReads(service), httpapi.WithWorkbenchGraph(service))
	u.setup.InteractiveAccess.SetBacklogReadAvailable(service.Backlog != nil)
	u.installWorkbenchWrites(service)
	if service.Repository != nil {
		u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithWorkbenchDocuments(service))
		u.setup.InteractiveAccess.SetRepositoryReadAvailable(true)
	}
	if u.setup.SessionBacklogReader == nil && u.credentialPlane != nil && u.credentialPlane.grants != nil {
		u.setup.SessionBacklogReader = workbenchSessionReader(service)
	}
}

func workbenchSessionReader(service *workbenchservice.Service) sessionops.ReaderFactory {
	return func(ctx context.Context, source sessionops.SourceContext) (sessionops.BacklogReader, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if source.Lease == nil {
			return nil, interactiveaccess.ErrDenied
		}
		if err := source.Lease.RequireSessionScope(source.Identity.Gaggle, source.Actor.Issuer, source.Actor.Subject); err != nil {
			return nil, err
		}
		if err := source.Lease.AuthorizeSourceRead("backlog.read"); err != nil {
			if errors.Is(err, interactiveaccess.ErrDenied) {
				return nil, nil
			}
			return nil, err
		}
		set, err := workbench.BindSources(source.RetainedGaggle)
		if err != nil {
			return nil, err
		}
		for _, binding := range set.Sources {
			if binding.Spec.Kind == "backlog" {
				return service.ForSession(&source.RetainedGaggle, source.Lease)
			}
		}
		return nil, nil
	}
}
