package main

import (
	"context"
	"errors"
	"fmt"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
)

// schedulerApplied is the running-generation accessor of the control role's
// configuration owner (cloudconfig.Configuration).
type schedulerApplied interface {
	Applied() (cloudconfig.Candidate, bool)
}

// constructScheduler composes the control role's scheduler admission: accepted
// starts go through the shared scheduler's run conditions, not straight to the
// start fence. It needs the configuration owner (applied generation), the
// journal backend (the scheduler's fenced instance log), the retained
// generations and the Temporal client, so it runs after those seams.
//
// The provider-backed counters (backlog, demand, refill, MaxOpenPRs) are NOT
// composed here yet: they need a provider opener with cloud credentials. Until
// then their gates are reported through the readiness checks and one journaled
// warning per epoch, and the workflows that need them stay disabled.
func constructScheduler(_ context.Context, db databaseServices, _ controlAuth, manager *lease.Manager, owner func() *lease.Handle, p *controlPlanes) error {
	applied, ok := p.Configuration.(schedulerApplied)
	switch {
	case !ok:
		return errors.New("control scheduler requires a configuration owner that reports its applied generation")
	case p.journalBackend == nil:
		return errors.New("control scheduler requires the journal backend (Journal seam)")
	case p.generations == nil:
		return errors.New("control scheduler requires the retained-generation source (Configuration seam)")
	case db.Temporal == nil:
		return errors.New("control scheduler requires the Temporal client")
	case db.NewClaims == nil:
		return errors.New("control scheduler requires the claims services")
	case manager == nil:
		return errors.New("control scheduler requires the controller lease")
	}
	quota := localscheduler.NewProviderQuotaState()
	gates := newGateStatus()
	p.schedulerGates = gates
	p.schedulerQuota = quota
	// ONE quota state: the scheduler gates on it, the counters feed it, and the
	// observations ledger persists it (the producer behind epochWiring).
	epochWiring.Observations = observationsFor(quota, func(err error) {
		if epochWiring.Report != nil {
			epochWiring.Report(err)
		}
	})
	confirmClosed := epoch.TemporalConfirmClosed(db.Temporal)
	// The terminal heal's provider reads are metered in the SAME state.
	epochWiring.Quota = func() *localscheduler.ProviderQuotaState { return quota }
	composition := schedulerComposition{
		Capacity: &epochParentCapacity,
		Applied:  applied.Applied,
		// One scheduler log per epoch, shared with the terminal hooks; its opener
		// retires it when the epoch ends.
		OpenLog: func(ctx context.Context, deps control.AdmissionDeps, report func(error)) (journal.InstanceStore, error) {
			return epochSchedulerLogs.For(ctx, p.journalBackend, deps.Handle, report)
		},
		State: func(deps control.AdmissionDeps) (localscheduler.SchedulerState, error) {
			services, err := db.NewClaims(manager, deps.Handle)
			if err != nil {
				return nil, err
			}
			return services, nil
		},
		OpenRuns: func(ctx context.Context, deps control.AdmissionDeps) ([]schedstart.OpenRun, error) {
			pins, err := deps.Store.UnmarkedRunPins(ctx)
			if err != nil {
				return nil, err
			}
			uncertain, err := uncertainStarts(ctx, deps.Store)
			if err != nil {
				return nil, err
			}
			return collectOpenRuns(ctx, openRunInputs{
				Pins: pins, Uncertain: uncertain, Closed: runClosedFunc(confirmClosed),
				// A parent parked on a child holds no slot, as locally. Strongly
				// consistent (committed journal events); an error leaves every slot held.
				Parked: p.journalBackend.ParkedOnChild,
				ParkedFailed: func(err error) {
					if epochWiring.Report != nil {
						epochWiring.Report(err)
					}
				},
			})
		},
		Terminal: schedstart.TemporalTerminal(db.Temporal),
		Entries: func(candidate cloudconfig.Candidate, counters generationCounters) ([]localscheduler.WorkflowEntry, error) {
			input := schedentries.Input{Generation: candidate.Digest, Config: candidate.Config, Set: candidate.Set, Pins: candidate.Pins}
			if counters != nil {
				input.Counters = counters.For
			}
			return schedentries.Build(input)
		},
		Sources: func(deps control.AdmissionDeps, build func(context.Context, startintent.Target) (startintent.Prepared, error)) (localscheduler.SourceQueue, error) {
			return schedentries.NewSources(schedentries.Deps{
				Queue: deps.Store, Generations: p.generations, Build: build,
				Report: func(err error) {
					if epochWiring.Report != nil {
						epochWiring.Report(fmt.Errorf("scheduler sources: %w", err))
					}
				},
			})
		},
		Quota: quota,
		Acquire: func(ctx context.Context, generation string) (func(), error) {
			held, err := p.generations.Acquire(ctx, generation)
			if err != nil {
				return nil, err
			}
			return func() {
				if err := held.Release(); err != nil && epochWiring.Report != nil {
					epochWiring.Report(fmt.Errorf("release generation %s: %w", generation, err))
				}
			}, nil
		},
		Gates: gates,
		Report: func(err error) {
			if epochWiring.Report != nil {
				epochWiring.Report(err)
			}
		},
	}
	p.schedulerComposition = composition
	p.Admission = composition.admission
	return nil
}
