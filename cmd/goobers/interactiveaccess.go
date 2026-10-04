package main

import (
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
)

func (u *upSession) configureInteractiveAccess() error {
	service, err := interactiveaccess.New(u.setup.Definitions.Gaggles, u.setup.Config.InteractiveCredentials, interactiveaccess.Dependencies{Stores: u.setup.SecretStores, Registrar: u.setup.SharedRegistry})
	if err != nil {
		return err
	}
	u.setup.InteractiveAccess = service
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
