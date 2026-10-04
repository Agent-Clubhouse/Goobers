package main

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func (u *upSession) installSessionPRRepair(factory workbenchservice.PRRepairFactory) {
	installed := factory != nil && u.durableTriggers != nil && u.durableTriggers.queue != nil && u.setup.RunnerRegistry != nil && u.credentialPlane != nil && u.credentialPlane.grants != nil
	u.setup.PRRepairCustody = newPRRepairCatalog(u.setup, installed)
	u.setup.InteractiveAccess.SetPRRepairAvailable(installed && u.setup.PRRepairCustody.snapshot.enabled)
	if !installed || u.setup.SessionPRRepair != nil {
		return
	}
	u.setup.SessionPRRepair = func(ctx context.Context, source sessionops.SourceContext) (sessionops.PRRepairer, error) {
		if source.Lease == nil || source.Identity.Session == nil {
			return nil, interactiveaccess.ErrDenied
		}
		if err := source.Lease.RequireSessionScope(source.Identity.Gaggle, source.Actor.Issuer, source.Actor.Subject); err != nil {
			return nil, err
		}
		turn, err := u.durableTriggers.queue.SessionTurn(ctx, source.Identity.Session.AcceptanceID)
		if err != nil {
			return nil, err
		}
		selection := turn.Message.RepairTarget
		if selection == nil {
			return nil, nil
		}
		repo := selection.Repository
		if err = source.Lease.AuthorizePRRepair(apiv1.InteractiveRepositoryIdentity{Provider: apiv1.Provider(repo.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name}); err != nil {
			if errors.Is(err, interactiveaccess.ErrDenied) {
				return nil, nil
			}
			return nil, err
		}
		custody := prRepairCustodian{layout: u.l, setup: u.setup, runID: source.Identity.RunID}
		service := &workbenchservice.PRRepairService{Queue: u.durableTriggers.queue, Client: factory, WithCustody: custody.scope, Scrubber: journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber())}
		return service.ForSession(ctx, &source.RetainedGaggle, source.Lease, source.Actor, source.Identity)
	}
}
