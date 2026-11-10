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
	"github.com/goobers/goobers/internal/localscheduler"
)

// epochWiring holds the Epoch parts other PRs compose. The Epoch seam is real
// and fail-closed: BuildEpoch refuses, naming what is missing, until every
// required part is present, so a controller can never serve with claims it
// cannot adopt or observations it cannot restore.
type epochWiringParts struct {
	// Claims builds the claim plane's epoch maintenance (claims.Plane,
	// p3/cloud-claims) bound to the epoch's fence and handle.
	Claims func(fence *lease.Manager, handle *lease.Handle) (epoch.Claims, error)
	// Terminal builds the epoch's terminal machinery: the leader-owned awaiters
	// that fire the terminal hooks, and the outbox delivery loop. When unset it is
	// composed from the control planes (terminalPart); a test or another
	// composition may set it.
	Terminal func(ctx context.Context, fence *lease.Manager, handle *lease.Handle) (terminalEpoch, error)
	// Observations supplies provider-observation restore/capture.
	Observations func() epoch.Observations
	// Report receives per-run awaiter failures.
	Report func(error)
	// Quota is the process's shared provider quota ledger (the scheduler's), so
	// the terminal hooks' provider reads are accounted in it. Optional until the
	// scheduler is composed; without it those reads are unmetered but slow.
	Quota func() *localscheduler.ProviderQuotaState
}

var epochWiring epochWiringParts

// epochTemporal is the dialed Temporal client, set by serveControl once it is
// connected (the Epoch seam is assembled before the dial).
var epochTemporal atomic.Pointer[epoch.Temporal]

func (p epochWiringParts) missing() []string {
	var missing []string
	if p.Claims == nil {
		missing = append(missing, "claims epoch maintenance")
	}
	if p.Observations == nil {
		missing = append(missing, "provider observations")
	}
	if p.Terminal == nil {
		missing = append(missing, "terminal hooks")
	}
	return missing
}

// constructEpoch composes the Epoch seam over the controller's direct pool. The
// fence is a second lease.Manager for the SAME lease: WithFence verifies the
// handle against the database, so it never acquires or renews anything.
func constructEpoch(_ context.Context, db databaseServices, _ controlAuth, _ *lease.Manager, _ func() *lease.Handle, p *controlPlanes) error {
	if db.Pools == nil || db.Pools.Direct == nil {
		return errors.New("epoch services require the controller's direct pool")
	}
	defaultTerminal := terminalPart(db, p)
	var defaultClaims func(*lease.Manager, *lease.Handle) (epoch.Claims, error)
	if plane, ok := p.Claims.(epoch.Claims); ok {
		// The claim plane takes its fence from the handle on every call.
		defaultClaims = func(*lease.Manager, *lease.Handle) (epoch.Claims, error) { return plane, nil }
	}
	p.Epoch = func(ctx context.Context, handle *lease.Handle) (control.EpochServices, error) {
		parts := epochWiring
		if parts.Terminal == nil {
			parts.Terminal = defaultTerminal
		}
		if parts.Claims == nil {
			parts.Claims = defaultClaims
		}
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
		term, err := parts.Terminal(ctx, fence, handle)
		if err != nil {
			return control.EpochServices{}, err
		}
		services, err := epoch.Build(epoch.Config{
			Handle: handle, Claims: claims,
			Cancel: cancelpg.Store{Fence: fence}, Terminal: terminalreportpg.Store{Fence: fence},
			Open: epoch.TemporalOpenRuns{Client: *temporal}, ConfirmClosed: epoch.TemporalConfirmClosed(*temporal), Awaiters: term.Awaiters, L3: term.Delivery,
			Observations: parts.Observations(), Report: parts.Report,
		})
		if err != nil {
			// Nothing started: withdraw the status the terminal part published.
			_ = term.Awaiters.Stop(ctx)
		}
		return services, err
	}
	return nil
}
