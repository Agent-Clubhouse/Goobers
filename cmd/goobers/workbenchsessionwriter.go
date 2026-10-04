package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchservice"
)

// workbenchSessionWriter is installed by the workbench host when native durable
// edits are available. Missing write policy leaves model/read-only turns usable.
func workbenchSessionWriter(service *workbenchservice.WriterService) sessionops.WriterFactory {
	return func(ctx context.Context, source sessionops.SourceContext) (sessionops.BacklogWriter, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if source.Lease == nil {
			return nil, interactiveaccess.ErrDenied
		}
		if err := source.Lease.RequireSessionScope(source.Identity.Gaggle, source.Actor.Issuer, source.Actor.Subject); err != nil {
			return nil, err
		}
		if err := source.Lease.AuthorizeSourceWrite("backlog.edit"); err != nil {
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
			if binding.Spec.Kind == "backlog" && binding.Spec.Writes != nil && len(binding.Spec.Writes.Fields) > 0 {
				return service.ForSession(&source.RetainedGaggle, source.Lease, source.Actor)
			}
		}
		return nil, nil
	}
}
