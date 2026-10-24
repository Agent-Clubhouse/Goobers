package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"time"

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
)

// terminalEpoch is one epoch's terminal machinery as the Epoch seam consumes it.
type terminalEpoch struct {
	Awaiters epoch.Awaiters
	// Delivery is the outbox delivery loop, run as the epoch's L3.
	Delivery func(context.Context) error
	// Status is the delivery loop's last observation.
	Status func() terminalcompose.Status
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
	return map[string]bool{"terminalOutbox": (*status)().Healthy}
}

// terminalOutboxLag is the control status page's view of the outbox backlog.
type terminalOutboxLag struct {
	Pending       int     `json:"pending"`
	OldestSeconds float64 `json:"oldestSeconds"`
	Healthy       bool    `json:"healthy"`
}

func currentTerminalOutboxLag() *terminalOutboxLag {
	status := terminalStatus.Load()
	if status == nil {
		return nil
	}
	s := (*status)()
	return &terminalOutboxLag{Pending: s.Pending, OldestSeconds: s.OldestAge.Seconds(), Healthy: s.Healthy}
}

// errTerminalInputs names what the control role is missing to compose the
// terminal hooks; the Epoch seam then refuses by name instead of serving without
// them.
var errTerminalInputs = errors.New("terminal hooks need the claims plane, configuration owner, journal backend, acceptance store, retained generations and Temporal")

// terminalPart returns the Terminal part composed from the control planes, or nil
// when an input is absent (the seam then reports the part missing).
func terminalPart(db databaseServices, p *controlPlanes) func(ctx context.Context, fence *lease.Manager, handle *lease.Handle) (terminalEpoch, error) {
	plane, okPlane := p.Claims.(*cloudclaims.Plane)
	backend, okBackend := p.journalReader.(terminalJournal)
	owner, okConfig := p.Configuration.(appliedConfiguration)
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
		return nil
	}
	runs := terminalwriter.JournalRuns{Pins: terminalwriter.AcceptancePins(store.RunPinsOf), Journal: backend, Generations: p.generations}
	writer, err := terminalwriter.New(runs, backends, registrar)
	if err != nil {
		return nil
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
		temporal := epochTemporal.Load()
		if temporal == nil {
			return terminalEpoch{}, errors.New("terminal hooks require the Temporal client")
		}
		comp, err := terminalcompose.New(terminalcompose.Deps{
			Handle: handle, Fence: fence, Pool: db.Pools.Transaction,
			Releaser: plane,
			Claimed:  claimedItemIDs(plane),
			Holders:  plane.ClaimedRuns,
			Temporal: *temporal,
			Open:     epoch.TemporalOpenRuns{Client: *temporal}.OpenRuns,
			Closed:   terminalawait.TemporalClosedRuns(*temporal),
			Pins: func(ctx context.Context, runID string) (terminalawait.RunPin, error) {
				pins, err := store.RunPinsOf(ctx, runID)
				return terminalawait.RunPin{Gaggle: pins.Gaggle, Workflow: pins.Workflow, RepoRef: pins.RepoRef}, err
			},
			Bind: runs.Bind, Writer: writer,
			Events: func(ctx context.Context, runID string) ([]journal.Event, error) {
				return runEvents(ctx, backend, runID)
			},
			Log: log, Report: report, Now: time.Now,
		})
		if err != nil {
			return terminalEpoch{}, err
		}
		return terminalEpoch{Awaiters: comp.Awaiters, Delivery: comp.Delivery.Tick, Status: comp.Delivery.Status}, nil
	}
}

// terminalJournal is the journal backend as the terminal hooks use it: the
// fenced scheduler log, and the run journal for binding and for reading a run's
// terminal events.
type terminalJournal interface {
	schedulerLogOpener
	terminalwriter.RunOpener
}

// appliedConfiguration is the configuration owner's applied generation.
type appliedConfiguration interface {
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
