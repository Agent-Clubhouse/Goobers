package sharedclaim

import (
	"context"
	"fmt"
)

// Visibility mirrors ownership for humans. It must change only the shared
// claimed label, never the coordination record or unrelated issue labels.
type Visibility interface {
	ReadClaimed(context.Context, string) (bool, error)
	SetClaimed(context.Context, string, bool) error
}

// ReconcileVisibility converges a non-authoritative label to the current lease.
// A post-write read repairs a delayed cleanup racing a successor. This is not
// a transaction with the label: a crash or failed write can leave drift until
// the next reconciliation. No path here mutates or releases ownership.
func ReconcileVisibility(ctx context.Context, store Store, labels Visibility, key string) error {
	if store == nil || labels == nil || !validKey(key) {
		return fmt.Errorf("invalid shared claim visibility reconciliation")
	}
	for range 3 {
		observed, err := store.Read(ctx, key)
		if err != nil {
			return err
		}
		if err := validateObservation(observed); err != nil {
			return err
		}
		// An absent shared record is not permission to touch a local marker.
		if observed.Revision == "" {
			return nil
		}
		want := visibleOwner(observed)
		present, err := labels.ReadClaimed(ctx, key)
		if err != nil {
			return err
		}
		if present != want {
			if err := labels.SetClaimed(ctx, key, want); err != nil {
				return err
			}
		}
		current, err := store.Read(ctx, key)
		if err != nil {
			return err
		}
		if err := validateObservation(current); err != nil {
			return err
		}
		if current.Revision == observed.Revision && visibleOwner(current) == want {
			return nil
		}
	}
	return fmt.Errorf("shared claim visibility did not stabilize: %w", ErrConflict)
}

func visibleOwner(observed Observation) bool {
	return observed.Record.Owner != (Owner{}) && observed.Now.Before(observed.Record.ExpiresAt)
}
