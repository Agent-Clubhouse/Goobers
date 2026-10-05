package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/internal/workbenchservice"
)

// workbenchSessionResolver installs only typed host-owned marker operations.
// Missing dedicated authority leaves ordinary model/read/edit turns usable.
func workbenchSessionResolver(service *workbenchservice.ResolutionService) sessionops.ResolverFactory {
	return func(ctx context.Context, source sessionops.SourceContext) (sessionops.BacklogResolver, error) {
		if source.Lease == nil {
			return nil, interactiveaccess.ErrDenied
		}
		if err := source.Lease.RequireSessionScope(source.Identity.Gaggle, source.Actor.Issuer, source.Actor.Subject); err != nil {
			return nil, err
		}
		if err := source.Lease.AuthorizeSourceResolve(); err != nil {
			if errors.Is(err, interactiveaccess.ErrDenied) {
				return nil, nil
			}
			return nil, err
		}
		bindings, err := workbench.BindSources(source.RetainedGaggle)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings.Sources {
			if binding.Spec.Kind == "backlog" && workbenchprovider.NeedsHumanResolutionAllowed(binding) {
				return service.ForSession(ctx, &source.RetainedGaggle, source.Lease, source.Actor, source.Identity)
			}
		}
		return nil, nil
	}
}
