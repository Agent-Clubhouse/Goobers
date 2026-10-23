package projectorpg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	"github.com/goobers/goobers/internal/journal"
)

// ServeConfig binds the projector lifecycle: hold the "projector" lease, and
// while holding it keep the read model current.
type ServeConfig struct {
	// Manager owns the "projector" lease (lease.Config.Name == LeaseName).
	Manager *lease.Manager
	// Pool is the controller database pool the projector reads the journal and
	// read model through; writes go through the lease manager's fence.
	Pool       *pgxpool.Pool
	Journal    journal.ReaderBackend
	InstanceID string
	// Notifier wakes feed subscribers after a commit. May be nil: the feed keeps
	// its polling fallback.
	Notifier *readmodelpg.Notifier
	// Wake delivers best-effort "a journal advanced" hints. May be nil.
	Wake <-chan struct{}
	// Report receives lease, epoch and per-run failures.
	Report func(error)
	// Health is shared across epochs and may be nil.
	Health *Health
	// Retry is the standby/backoff base interval (default 2s, capped at 1m).
	Retry time.Duration
}

// Serve runs until ctx ends. It stays a standby while another replica holds the
// lease, projects as the holder, and on any authority loss or failure drops the
// epoch and tries again, with exponential backoff for real failures and the
// plain retry interval for contention. Cancellation releases the lease.
func Serve(ctx context.Context, config ServeConfig) error {
	if config.Manager == nil || config.Pool == nil || config.Journal == nil || config.InstanceID == "" {
		return errors.New("projectorpg: manager, pool, journal and instance are required")
	}
	report := config.Report
	if report == nil {
		report = func(error) {}
	}
	base := config.Retry
	if base <= 0 {
		base = 2 * time.Second
	}
	backoff := base
	for ctx.Err() == nil {
		err := config.Manager.Run(ctx, func(ctx context.Context, handle *lease.Handle) error {
			projector, err := readmodelpg.NewProjector(ctx, config.Pool, config.Manager, handle, config.Notifier)
			if err != nil {
				return err
			}
			runner, err := New(Config{Projector: projector, Journal: config.Journal, InstanceID: config.InstanceID, Wake: config.Wake, Report: report, Health: config.Health})
			if err != nil {
				return err
			}
			return runner.Run(ctx)
		})
		if ctx.Err() != nil {
			return nil
		}
		delay := base
		if err != nil && !errors.Is(err, lease.ErrHeld) {
			report(err)
			delay = backoff
			backoff = min(backoff*2, max(time.Minute, base))
		} else {
			backoff = base
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
