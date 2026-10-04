package main

import (
	"github.com/goobers/goobers/internal/childmonitor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
)

func (u *upSession) configureInteractiveAccess() error {
	service, err := interactiveaccess.New(u.setup.Definitions.Gaggles, u.setup.Config.InteractiveCredentials, interactiveaccess.Dependencies{Stores: u.setup.SecretStores, Registrar: u.setup.SharedRegistry})
	if err != nil {
		return err
	}
	u.setup.InteractiveAccess = service
	if u.setup.RunnerRegistry != nil {
		if u.setup.InteractiveRestartExecution == nil {
			u.setup.InteractiveRestartExecution = u.setup.buildInteractiveRestartExecution
		}
		if u.setup.InteractiveRestartRecovery == nil {
			u.setup.InteractiveRestartRecovery = u.setup.buildInteractiveRestartIdentity
		}
	}
	if u.setup.InteractiveRestartRecovery != nil && u.setup.RunnerRegistry != nil {
		u.setup.RunnerRegistry.setInteractiveGenerationResolver(interactiveGenerationResolver(u.l, u.setup, u.setup.InteractiveRestartRecovery))
	}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithInteractivePermissions(service))
	return nil
}

func (r *configReloader) publishInteractiveDefinitions(definitions *instance.ConfigSet, publish func() error) error {
	if r.setup.InteractiveAccess == nil {
		return r.publishChildDefinitions(definitions, publish)
	}
	// Lock ordering is interactive policy, child authority, then scheduler. An
	// old-policy provider callback completes before the new catalog is visible.
	return r.setup.InteractiveAccess.Apply(definitions.Gaggles, func() error { return r.publishChildDefinitions(definitions, publish) })
}

func (u *upSession) configureInteractiveRuns(messages httpapi.OperatorMessageService) error {
	service, err := intervention.NewHumanService(u.interventions, u.setup.InteractiveAccess, messages, journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber()))
	if err != nil {
		return err
	}
	if u.setup.InteractiveRestartExecution != nil {
		adapter := &interactiveStageRestart{layout: u.l, setup: u.setup, service: u.interventions}
		service.AttachStageRestarts(adapter)
		if u.setup.ChildRestarts != nil {
			u.setup.ChildRestarts.restart = adapter.reconcileChildRestart
		}
		u.setup.InteractiveAccess.SetStageRestartAvailable(true)
	}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithInteractiveRuns(service))
	if u.durableTriggers != nil {
		monitor := &childmonitor.Service{Layout: u.l, Queue: u.durableTriggers.queue, Permissions: u.setup.InteractiveAccess, Scrubber: journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber()), Observe: u.childPublicationObservation()}
		u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithChildWorkflowMonitor(monitor), httpapi.WithChildPublicationChecks(monitor))
	}
	u.configureWorkbenchReads()
	return u.configureInteractiveSessions()
}
