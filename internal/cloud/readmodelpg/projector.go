package readmodelpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/readmodel"
)

// Projector commits projections under the dedicated projector lease and wakes
// the durable feed only after the fenced transaction commits.
type Projector struct {
	store    *Store
	notifier *Notifier
}

// NewProjector binds projection commits to a live "projector" lease handle.
func NewProjector(
	ctx context.Context,
	pool *pgxpool.Pool,
	manager *cloudlease.Manager,
	handle *cloudlease.Handle,
	notifier *Notifier,
) (*Projector, error) {
	if handle == nil || handle.Name != "projector" {
		return nil, errors.New("readmodelpg: projector lease handle is required")
	}
	fence, err := NewLeaseFence(manager, handle)
	if err != nil {
		return nil, err
	}
	store, err := New(ctx, pool, fence)
	if err != nil {
		return nil, err
	}
	store.feed = readmodel.NewFeedWithWakeup(
		NewActiveFeedStore(store.pool), notifier)
	return &Projector{store: store, notifier: notifier}, nil
}

// Commit applies one projection and then publishes a latency wakeup.
func (p *Projector) Commit(ctx context.Context, projection readmodel.Projection) error {
	if err := p.store.UpsertRun(ctx, projection); err != nil {
		return err
	}
	if p.notifier != nil {
		_ = p.notifier.Notify(ctx)
	}
	return nil
}

// Feed returns a restart-safe durable feed with a one-second polling fallback.
func (p *Projector) Feed() *readmodel.ChangeFeed { return p.store.feed }

// Store exposes the backend for read paths.
func (p *Projector) Store() *Store { return p.store }
