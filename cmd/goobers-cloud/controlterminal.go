package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudclaims "github.com/goobers/goobers/internal/cloud/claims"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/terminalawait"
	"github.com/goobers/goobers/internal/cloud/terminalcompose"
	"github.com/goobers/goobers/internal/cloud/terminalwriter"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

// terminalEpoch is one epoch's terminal machinery as the Epoch seam consumes it.
type terminalEpoch struct {
	Awaiters epoch.Awaiters
	// Delivery is the outbox delivery loop, run as the epoch's L3.
	Delivery func(context.Context) error
}

// terminalStatus is the live epoch's delivery status, for readiness and the
// control status page. Nil between epochs.
var terminalStatus atomic.Pointer[func() terminalcompose.Status]

// terminalOutboxChecks reports the terminal-report outbox as a readiness check:
// healthy while the oldest undelivered report has waited less than the bound.
// It is informational (it does not gate Ready): a provider outage must not pull
// the controller out of rotation, but it must be visible.
func terminalOutboxChecks() map[string]bool {
	status := terminalStatus.Load()
	if status == nil {
		return nil
	}
	current := (*status)()
	return map[string]bool{"terminalOutbox": current.Healthy, "blockedCycleEscalation": len(current.RefusedCycles) == 0 && !current.RefusedUnknown}
}

// terminalOutboxLag is the control status page's view of the outbox backlog.
type terminalOutboxLag struct {
	Pending       int     `json:"pending"`
	OldestSeconds float64 `json:"oldestSeconds"`
	Healthy       bool    `json:"healthy"`
	// DeferredCycles is the blocked-record cycles awaiting re-verification;
	// RefusedCycles those whose escalation the provider permanently refused.
	DeferredCycles int      `json:"deferredCycles"`
	RefusedCycles  []string `json:"refusedCycles,omitempty"`
}

func currentTerminalOutboxLag() *terminalOutboxLag {
	status := terminalStatus.Load()
	if status == nil {
		return nil
	}
	s := (*status)()
	return &terminalOutboxLag{Pending: s.Pending, OldestSeconds: s.OldestAge.Seconds(), Healthy: s.Healthy, DeferredCycles: s.DeferredCycles, RefusedCycles: s.RefusedCycles}
}

// terminalPart returns the Terminal part composed from the control planes, or nil
// when an input is absent (the Epoch seam then refuses, naming "terminal hooks":
// it needs the claims plane, configuration owner, journal backend, acceptance
// store, retained generations and Temporal).
func terminalPart(db databaseServices, p *controlPlanes) func(ctx context.Context, fence *lease.Manager, handle *lease.Handle) (terminalEpoch, error) {
	plane, okPlane := p.Claims.(*cloudclaims.Plane)
	backend, okBackend := p.journalReader.(terminalJournal)
	owner, okConfig := p.Configuration.(terminalConfiguration)
	store, okStore := db.Acceptances.(*acceptance.Store)
	if !okPlane || !okBackend || !okConfig || !okStore || p.generations == nil || db.Temporal == nil || db.Pools == nil {
		return nil
	}
	registrar := journal.NewRegistryScrubber()
	backends, err := terminalwriter.NewConfiguredBackends(func() (*instance.Config, string, bool) {
		candidate, ok := owner.Applied()
		cfg := owner.InstanceConfig()
		return cfg, candidate.Digest, ok && cfg != nil
	}, registrar, func(cfg *instance.Config) (credentials.StoreResolver, error) {
		return secretstore.NewRegistry(cfg.SecretStores)
	})
	if err != nil {
		return failedTerminalPart(fmt.Errorf("terminal hooks: provider credentials: %w", err))
	}
	runs := terminalwriter.JournalRuns{Pins: terminalwriter.AcceptancePins(store.RunPinsOf), Journal: backend, Generations: p.generations}
	writer, err := terminalwriter.New(runs, backends, registrar)
	if err != nil {
		return failedTerminalPart(fmt.Errorf("terminal hooks: provider writer: %w", err))
	}
	return func(ctx context.Context, fence *lease.Manager, handle *lease.Handle) (terminalEpoch, error) {
		report := func(err error) {
			if epochWiring.Report != nil {
				epochWiring.Report(err)
			}
		}
		log, err := epochSchedulerLogs.For(ctx, backend, handle, report)
		if err != nil {
			return terminalEpoch{}, err
		}
		// The sweep's escalation is the one credentialed write outside a run: it
		// journals every write to the epoch's fenced log before making it.
		escalator, err := terminalwriter.NewInstanceWriter(func() (*instance.ConfigSet, string, bool) {
			candidate, ok := owner.Applied()
			return candidate.Set, candidate.Digest, ok
		}, backends, registrar, log.Append)
		if err != nil {
			return terminalEpoch{}, fmt.Errorf("terminal hooks: instance writer: %w", err)
		}
		comp, err := terminalcompose.New(terminalcompose.Deps{
			Escalator: escalator,
			Handle:    handle, Fence: fence, Pool: db.Pools.Transaction,
			Releaser: plane,
			Claimed:  claimedItemIDs(plane),
			Holders:  plane.ClaimedRuns,
			Temporal: db.Temporal,
			Open:     epoch.TemporalOpenRuns{Client: db.Temporal}.OpenRuns,
			Closed:   terminalawait.TemporalClosedRuns(db.Temporal),
			Pins: func(ctx context.Context, runID string) (terminalawait.RunPin, error) {
				pins, err := store.RunPinsOf(ctx, runID)
				return terminalawait.RunPin{Gaggle: pins.Gaggle, Workflow: pins.Workflow, RepoRef: pins.RepoRef}, err
			},
			Bind: runs.Bind, Writer: writer,
			Backends: backends, Instance: instanceBinding(owner), Scrubber: registrar, Reserve: quotaReserver(),
			Events: func(ctx context.Context, runID string) ([]journal.Event, error) {
				return runEvents(ctx, backend, runID)
			},
			Log: log, Report: report, Now: time.Now,
		})
		if err != nil {
			return terminalEpoch{}, err
		}
		// Publish this epoch's status for readiness and the status page; its
		// awaiters withdraw it when the epoch stops.
		status := comp.Delivery.Status
		terminalStatus.Store(&status)
		return terminalEpoch{Awaiters: &clearingAwaiters{Awaiters: comp.Awaiters, published: &status}, Delivery: comp.Delivery.Tick}, nil
	}
}

// terminalJournal is the journal backend as the terminal hooks use it: the
// fenced scheduler log, and the run journal for binding and for reading a run's
// terminal events.
type terminalJournal interface {
	schedulerLogOpener
	terminalwriter.RunOpener
}

// terminalConfiguration is the configuration owner's applied generation.
type terminalConfiguration interface {
	Applied() (cloudconfig.Candidate, bool)
	InstanceConfig() *instance.Config
}

// terminalReadTimeout bounds one claim-ledger read made without a context.
const terminalReadTimeout = 15 * time.Second

// claimedItemIDs lists the items a run holds, from the claim plane. A database
// failure is an error, never an empty list.
func claimedItemIDs(plane *cloudclaims.Plane) func(string) ([]string, error) {
	return func(runID string) ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), terminalReadTimeout)
		defer cancel()
		return plane.ClaimedItems(ctx, runID)
	}
}

// runEvents reads a run's committed journal events. A run with no journal has no
// events.
func runEvents(ctx context.Context, backend terminalwriter.RunOpener, runID string) ([]journal.Event, error) {
	reader, err := backend.OpenRun(ctx, journal.RunRef{ID: runID, ReadOnly: true})
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, journal.ErrNotRunDirectory) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return reader.Events()
}

// clearingAwaiters withdraws the published delivery status when its epoch stops,
// so a deposed or standby replica reports no outbox state of its own.
type clearingAwaiters struct {
	epoch.Awaiters
	published *func() terminalcompose.Status
}

func (a *clearingAwaiters) Stop(ctx context.Context) error {
	err := a.Awaiters.Stop(ctx)
	// Only our own publication: a successor epoch on this process may already
	// have stored its own.
	terminalStatus.CompareAndSwap(a.published, nil)
	return err
}

// failedTerminalPart is a Terminal part that reports why composition failed, so
// the Epoch seam refuses with the cause instead of only naming a missing part.
func failedTerminalPart(err error) func(context.Context, *lease.Manager, *lease.Handle) (terminalEpoch, error) {
	return func(context.Context, *lease.Manager, *lease.Handle) (terminalEpoch, error) {
		return terminalEpoch{}, err
	}
}

// instanceBinding binds a repository through the applied configuration, for the
// reads that must not depend on a run's pin.
func instanceBinding(owner terminalConfiguration) func(providers.RepositoryRef) (terminalwriter.Binding, bool) {
	return func(repo providers.RepositoryRef) (terminalwriter.Binding, bool) {
		candidate, ok := owner.Applied()
		if !ok {
			return terminalwriter.Binding{}, false
		}
		return terminalwriter.InstanceBinding(candidate.Set, candidate.Digest, repo)
	}
}

// quotaReserver accounts a provider read in the shared quota ledger when the
// scheduler has composed one; false skips the read this pass.
func quotaReserver() func(providers.ProviderKind) bool {
	return func(provider providers.ProviderKind) bool {
		if epochWiring.Quota == nil {
			return true
		}
		state := epochWiring.Quota()
		if state == nil {
			return true
		}
		// The current window only: ReservePolls would retire an expired window and
		// hand the one-time reset to this caller, which would drop it and cost the
		// scheduler its reset journal and resume pacing.
		return state.ReserveCurrentPolls(apiv1.Provider(provider), 1).Allowed > 0
	}
}
