package acceptance

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// startCustody maps the store's durable authority onto the current bound lease.
// Reading a predecessor never adopts it; only a classified start may do that.
type startCustody struct{ store *Store }

func (s *startCustody) ReadStart(ctx context.Context, id string) (engine.StartIdentity, error) {
	if err := s.CheckStartLease(ctx); err != nil {
		return engine.StartIdentity{}, err
	}
	authority, err := s.store.ReadStartAuthority(ctx, id)
	if errors.Is(err, triggerqueue.ErrTransition) {
		if err = s.store.BeginDispatch(ctx, id); err == nil {
			authority, err = s.store.ReadStartAuthority(ctx, id)
		}
	}
	if err != nil {
		return engine.StartIdentity{}, err
	}
	return engine.StartIdentity{
		AcceptanceID: authority.AcceptanceID, RunID: authority.RunID, InstanceID: s.store.instanceID,
		OwnerEpoch: s.store.handle.Epoch, StarterID: s.store.handle.Holder, PredecessorEpoch: authority.OwnerEpoch,
	}, nil
}

func (s *startCustody) ConfirmStart(ctx context.Context, identity engine.StartIdentity, _ bool, pins RunPins) error {
	if err := s.CheckStartLease(ctx); err != nil {
		return err
	}
	if identity.InstanceID != s.store.instanceID || identity.OwnerEpoch != s.store.handle.Epoch || identity.StarterID != s.store.handle.Holder {
		return triggerqueue.ErrTransition
	}
	// Preserve the exact epoch observed before Temporal; a new read must not
	// silently authorize a different predecessor after classification.
	if identity.PredecessorEpoch < 1 || identity.PredecessorEpoch > identity.OwnerEpoch {
		return triggerqueue.ErrTransition
	}
	if identity.PredecessorEpoch < identity.OwnerEpoch {
		return s.store.AdoptStart(ctx, identity.AcceptanceID, identity.RunID, identity.PredecessorEpoch, pins)
	}
	return s.store.ConfirmStart(ctx, identity.AcceptanceID, identity.RunID, pins)
}

func (s *startCustody) CheckStartLease(ctx context.Context) error {
	if s.store == nil || s.store.handle == nil || s.store.handle.InstanceID != s.store.instanceID {
		return triggerqueue.ErrTransition
	}
	return s.store.withFence(ctx, func(context.Context, pgx.Tx, int64) error { return nil })
}
