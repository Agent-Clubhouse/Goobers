package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/goobers/goobers/internal/cloud/cancelpg"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
)

// epochWiring holds the Epoch parts other PRs compose. The Epoch seam is real
// and fail-closed: BuildEpoch refuses, naming what is missing, until every
// required part is present, so a controller can never serve with claims it
// cannot adopt or observations it cannot restore.
type epochWiringParts struct {
	// Claims builds the claim plane's epoch maintenance (claims.Plane,
	// p3/cloud-claims) bound to the epoch's fence and handle.
	Claims func(fence *lease.Manager, handle *lease.Handle) (epoch.Claims, error)
	// Awaiters builds the leader-owned terminal-effect awaiters (optional until
	// the terminal hooks are composed).
	Awaiters func(fence *lease.Manager, handle *lease.Handle) (epoch.Awaiters, error)
	// Observations supplies provider-observation restore/capture.
	Observations func() epoch.Observations
	// Report receives per-run awaiter failures.
	Report func(error)
}

var epochWiring epochWiringParts

// epochTemporal is the dialed Temporal client, set by serveControl once it is
// connected (the Epoch seam is assembled before the dial).
var epochTemporal atomic.Pointer[epoch.Lister]

func (p epochWiringParts) missing() []string {
	var missing []string
	if p.Claims == nil {
		missing = append(missing, "claims epoch maintenance")
	}
	if p.Observations == nil {
		missing = append(missing, "provider observations")
	}
	return missing
}

// constructEpoch composes the Epoch seam over the controller's direct pool. The
// fence is a second lease.Manager for the SAME lease: WithFence verifies the
// handle against the database, so it never acquires or renews anything.
func constructEpoch(_ context.Context, db databaseServices, _ controlAuth, _ func() *lease.Handle, p *controlPlanes) error {
	if db.Pools == nil || db.Pools.Direct == nil {
		return errors.New("epoch services require the controller's direct pool")
	}
	p.Epoch = func(_ context.Context, handle *lease.Handle) (control.EpochServices, error) {
		parts := epochWiring
		if missing := parts.missing(); len(missing) > 0 {
			return control.EpochServices{}, fmt.Errorf("cloud epoch services are not composed yet: %s", strings.Join(missing, ", "))
		}
		temporal := epochTemporal.Load()
		if temporal == nil {
			return control.EpochServices{}, errors.New("cloud epoch services require the Temporal client")
		}
		fence, err := lease.New(lease.Config{Pool: db.Pools.Direct, InstanceID: handle.InstanceID, Name: handle.Name, Holder: handle.Holder})
		if err != nil {
			return control.EpochServices{}, err
		}
		claims, err := parts.Claims(fence, handle)
		if err != nil {
			return control.EpochServices{}, err
		}
		var awaiters epoch.Awaiters
		if parts.Awaiters != nil {
			if awaiters, err = parts.Awaiters(fence, handle); err != nil {
				return control.EpochServices{}, err
			}
		}
		return epoch.Build(epoch.Config{
			Handle: handle, Claims: claims,
			Cancel: cancelpg.Store{Fence: fence}, Terminal: terminalreportpg.Store{Fence: fence},
			Open: epoch.TemporalOpenRuns{Client: *temporal}, Awaiters: awaiters,
			Observations: parts.Observations(), Report: parts.Report,
		})
	}
	return nil
}
