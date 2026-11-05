package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/triggers"
)

// triggerPin supplies the immutable workflow and goober digests an ordinary
// trigger is admitted against. It stays nil until the start-input resolver
// composition (#7818) lands; the plane then answers 503 so callers retry with
// the same key, and nothing is recorded.
var triggerPin triggers.Pin

// appliedConfiguration is what the trigger plane needs of the process-wide
// Configuration: its engine-only guard, read as one snapshot with the applied generation digest.
type appliedConfiguration interface {
	AdmitApplied(gaggle, workflow string) (string, error)
}

// constructTriggers composes the API-side trigger plane over the API pool's
// instance-bound acceptance store (goobers_api, no controller credentials).
// Acceptance is its whole job: the single-active controller drains accepted
// rows through the start fence. The Configuration is resolved on each request,
// so assembly order against that seam is free; until it names a generation the
// plane fails closed.
func constructTriggers(_ context.Context, db databaseServices, _ controlAuth, _ *lease.Manager, _ func() *lease.Handle, p *controlPlanes) error {
	if db.Acceptances == nil {
		return errors.New("trigger plane requires the API acceptance store")
	}
	p.Triggers = &triggers.Service{
		Store: db.Acceptances,
		Admission: func() triggers.Admission {
			if applied, ok := p.Configuration.(appliedConfiguration); ok {
				return applied
			}
			return nil
		},
		Pin: triggerPin,
	}
	return nil
}
