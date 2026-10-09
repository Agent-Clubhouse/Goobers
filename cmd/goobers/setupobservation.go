package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readmodel/projector"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// schedulerObservation owns setup's telemetry and derived read stores. The
// constructor rolls back partial construction; successful setup transfers this
// owner to schedulerSetup, whose shutdown keeps the established flush order.
type schedulerObservation struct {
	tel                      *telemetry.Client
	rollupDB                 *rollup.DB
	readModel                *readmodel.Store
	readModelEpoch           string
	watermarks               *intake.Store
	stopProjector            func()
	retentionStats           func() readmodel.RetentionStats
	projectorStats           func() projector.Stats
	projectorRestartComplete bool
	instanceLog              *journal.InstanceLog
	telemetryExporterHealth  *telemetry.ExporterHealth
	closeOnce                sync.Once
	steps                    []shutdownStep
}

func openSchedulerObservation(ctx context.Context, l instance.Layout, cfg *instance.Config, sharedScrubber journal.Scrubber, sharedReg *journal.RegistryScrubber, secretStores *secretstore.Registry, options schedulerSetupOptions) (*schedulerObservation, error) {
	owned := &schedulerObservation{telemetryExporterHealth: newTelemetryExporterHealth(cfg)}
	if err := owned.open(ctx, l, cfg, sharedScrubber, sharedReg, secretStores, options); err != nil {
		return nil, err
	}
	return owned, nil
}

// open constructs into this owner so each acquired resource is registered before
// the next fallible operation. A failed owner is closed and must not be reused.
func (owned *schedulerObservation) open(ctx context.Context, l instance.Layout, cfg *instance.Config, sharedScrubber journal.Scrubber, sharedReg *journal.RegistryScrubber, secretStores *secretstore.Registry, options schedulerSetupOptions) (err error) {
	defer func() {
		if err != nil {
			_ = owned.Close()
		}
	}()
	var telemetryOTLPDegradeErr error
	if cfg.TelemetryEnabled() {
		reportStartupProgress(options.startupProgress, "opening telemetry state")
		owned.tel, err = buildTelemetryClient(ctx, l, sharedScrubber, sharedReg, cfg.Telemetry, secretStores, owned.telemetryExporterHealth, options.telemetryReplayStart)
		if err != nil {
			if !errors.Is(err, telemetry.ErrOTLPUnavailable) {
				return err
			}
			// owned.tel is still a valid, usable client (local-only — see
			// ErrOTLPUnavailable's doc); warn now on stderr — the daemon has
			// no instance log open yet, and `kubectl logs` is the only place
			// an operator watching a rollout will see this — then park the
			// cause to also log loudly once owned.instanceLog opens, matching the
			// other non-fatal degrades in this function.
			fmt.Fprintf(os.Stderr, "warning: otlp telemetry unavailable, continuing local-only: %v\n", err)
			telemetryOTLPDegradeErr = err
			err = nil
		}
		owned.rollupDB, err = rollup.Open(l.TelemetryDB())
		if err != nil {
			return err
		}
	}
	// Derived read-model failures are best effort and independent of telemetry.
	// Discard orphaned rebuilds to release their change-retention pins. The state
	// smoke check uses Background so caller cancellation cannot invalidate a store.
	if discarded, discardErr := readmodel.DiscardStaleRebuilds(filepath.Dir(l.ReadDB())); discardErr != nil {
		fmt.Fprintf(os.Stderr, "warning: discard stale read-model rebuilds: %v\n", discardErr)
	} else if discarded > 0 {
		fmt.Fprintf(os.Stderr, "discarded %d orphaned read-model rebuild(s)\n", discarded)
	}
	reportStartupProgress(options.startupProgress, "opening read-model state")
	if readStore, readErr := readmodel.Open(l.ReadDB()); readErr != nil {
		fmt.Fprintf(os.Stderr, "warning: open read model: %v\n", readErr)
	} else if state, stateErr := readStore.State(context.Background()); stateErr != nil {
		// Reading the state back turns "the file opened" into "the schema is
		// at the version this build expects and the store has an epoch",
		// which is the difference between finding a broken store now and
		// finding it on the first read after the projector lands.
		fmt.Fprintf(os.Stderr, "warning: read model state: %v\n", stateErr)
		_ = readStore.Close()
	} else {
		// Measurement flags (#1782). The four population filters are derived
		// from the telemetry rollup, which no journal event carries, so the
		// read model needs a source for them or `population=` can only ever
		// match nothing.
		//
		// Attached before the first projection rather than after: a run
		// projected without a source has its flags cleared, and nothing later
		// re-projects it. A nil owned.rollupDB detaches, which is correct for a
		// telemetry-disabled instance -- there, the population filters have no
		// data to be right about.
		if owned.rollupDB != nil {
			readStore.WithMeasurement(readservice.NewTelemetryMeasurement(owned.rollupDB))
		}
		// §6.6 step 2: build it by rebuild-from-journals on first start.
		// A completed store is kept and updated incrementally by the writer
		// seam; an unready store is rebuilt synchronously before attachment,
		// and a ready one re-projects only rows from older rules (#6895).
		if err := buildReadModelIfNeeded(ctx, readStore, state, l, options.startupProgress); err != nil {
			fmt.Fprintf(os.Stderr, "warning: build read model: %v\n", err)
			_ = readStore.Close()
		} else {
			owned.readModelEpoch = state.Epoch
			owned.readModel = readStore

			// The projector (#1923). Opening intake and starting the commit loop
			// is what inverts the coupling: from here the writer records a
			// watermark and forgets, and this owns discovery and application.
			if intakeStore, intakeErr := intake.Open(l.IntakeDB()); intakeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: open intake store: %v\n", intakeErr)
			} else {
				owned.watermarks = intakeStore
				owned.stopProjector, owned.retentionStats, owned.projectorStats, owned.projectorRestartComplete = startProjector(ctx, readStore, intakeStore, l, cfg)
			}
		}
	}

	owned.instanceLog, _, err = journal.OpenInstanceLog(l.SchedulerDir(), journal.WithScrubber(sharedScrubber), journal.WithInstanceAppendDropObserver(owned.tel))
	if err != nil {
		return fmt.Errorf("open instance log: %w", err)
	}
	owned.telemetryExporterHealth.AttachInstanceLog(owned.instanceLog)
	if telemetryOTLPDegradeErr != nil {
		logTelemetryOTLPUnavailable(owned.instanceLog, telemetryOTLPDegradeErr)
	}
	return nil
}

// Close rolls back partially opened resources. Normal shutdown uses the same
// idempotent steps after its local flush and final scheduler telemetry ingest.
func (owned *schedulerObservation) Close() error {
	if owned == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runShutdownSteps(ctx, owned.closeSteps(ctx))
}

func (owned *schedulerObservation) closeSteps(ctx context.Context) []shutdownStep {
	owned.closeOnce.Do(func() {
		if owned.tel != nil {
			owned.steps = append(owned.steps, shutdownStep{"telemetry client", func() error { return owned.tel.Shutdown(ctx) }})
		}
		if owned.rollupDB != nil {
			owned.steps = append(owned.steps, shutdownStep{"telemetry rollup database", owned.rollupDB.Close})
		}
		// Stop the commit loop before either of its databases is closed.
		if owned.stopProjector != nil {
			owned.steps = append(owned.steps, shutdownStep{"read model projector", func() error { owned.stopProjector(); return nil }})
		}
		if owned.readModel != nil {
			owned.steps = append(owned.steps, shutdownStep{"read model store", owned.readModel.Close})
		}
		if owned.watermarks != nil {
			owned.steps = append(owned.steps, shutdownStep{"source watermark store", owned.watermarks.Close})
		}
		if owned.instanceLog != nil {
			owned.steps = append(owned.steps, shutdownStep{"instance log", owned.instanceLog.Close})
		}
		for i := range owned.steps {
			owned.steps[i].run = sync.OnceValue(owned.steps[i].run)
		}
	})
	return owned.steps
}
