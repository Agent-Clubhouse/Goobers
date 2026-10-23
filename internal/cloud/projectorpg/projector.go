// Package projectorpg keeps the PostgreSQL read model current with the PG
// journal. It runs under its own "projector" lease, separate from the controller
// lease, so projection throughput can never starve admission and a lost
// projector lease never fences the controller.
//
// Correctness never depends on a notification: every pass asks the read model
// which runs have journal sequences beyond their source watermark, so a restart,
// a missed wakeup or a long outage converges on the next pass. Wakeups (the
// journal observer) and the poll interval only bound latency.
package projectorpg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// LeaseName is the projector lease's name.
const LeaseName = "projector"

const (
	// DefaultBatch bounds the runs projected per catch-up page.
	DefaultBatch = 50
	// DefaultPoll is the catch-up cadence when no wakeup arrives; consumers of
	// the journal observer keep this polling fallback.
	DefaultPoll = time.Second
)

// Config binds one projector epoch.
type Config struct {
	// Projector commits projections under the live projector lease handle.
	Projector *readmodelpg.Projector
	// Journal reads committed run history; the PG journal backend.
	Journal    journal.ReaderBackend
	InstanceID string
	// Wake delivers best-effort "a journal advanced" hints. May be nil.
	Wake <-chan struct{}
	// Batch and Poll default to DefaultBatch and DefaultPoll.
	Batch int
	Poll  time.Duration
	// Report receives per-run projection failures that do not stop the loop.
	Report func(error)
}

// Runner projects journal history into the read model for one lease epoch.
type Runner struct {
	config Config
	// failing remembers the last reported failure per run, so a run that stays
	// unprojectable is retried every pass but reported once per distinct error.
	failing map[string]string
}

// New validates the configuration.
func New(config Config) (*Runner, error) {
	if config.Projector == nil || config.Journal == nil || config.InstanceID == "" {
		return nil, errors.New("projectorpg: projector, journal and instance are required")
	}
	if config.Batch == 0 {
		config.Batch = DefaultBatch
	}
	if config.Poll == 0 {
		config.Poll = DefaultPoll
	}
	if config.Batch < 1 || config.Poll < 0 {
		return nil, errors.New("projectorpg: batch and poll must be positive")
	}
	if config.Report == nil {
		config.Report = func(error) {}
	}
	return &Runner{config: config, failing: map[string]string{}}, nil
}

// Run catches up, then repeats on every wakeup or poll until ctx ends. Authority
// loss (a stale lease or a newer model owner) ends the run so the lease host
// drops the epoch; any other failure of a single run is reported and retried on
// the next pass.
func (r *Runner) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.config.Wake:
		case <-timer.C:
		}
		if _, err := r.Sync(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		timer.Reset(r.config.Poll)
	}
}

// Sync runs one catch-up pass over every stale run and reports how many were
// projected.
func (r *Runner) Sync(ctx context.Context) (int, error) {
	projected, after := 0, ""
	for {
		stale, err := r.config.Projector.Store().StaleRuns(ctx, r.config.InstanceID, after, r.config.Batch)
		if err != nil {
			return projected, err
		}
		for _, run := range stale {
			after = run.RunID
			if err := r.project(ctx, run.RunID); err != nil {
				if isAuthorityLoss(err) {
					return projected, err
				}
				if r.failing[run.RunID] != err.Error() {
					r.failing[run.RunID] = err.Error()
					r.config.Report(fmt.Errorf("project run %s: %w", run.RunID, err))
				}
				continue
			}
			delete(r.failing, run.RunID)
			projected++
		}
		if len(stale) < r.config.Batch {
			return projected, nil
		}
	}
}

func (r *Runner) project(ctx context.Context, runID string) error {
	reader, err := r.config.Journal.OpenRun(ctx, journal.RunRef{ID: runID, ReadOnly: true})
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
	if err != nil {
		return err
	}
	return r.config.Projector.Commit(ctx, projection)
}

func isAuthorityLoss(err error) bool {
	return errors.Is(err, lease.ErrStale) || errors.Is(err, lease.ErrSelfFenced) ||
		errors.Is(err, readmodelpg.ErrStaleOwner) || errors.Is(err, readmodelpg.ErrVersionMismatch)
}
