// Package noworkstreakpg stores the repeated-no-work counters (#5379) in
// PostgreSQL for the cloud controller's completed-terminal hook.
package noworkstreakpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/providers"
)

// Store keeps one record per item under the controller-lease fence.
type Store struct {
	Fence *lease.Manager
	Now   func() time.Time
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// lockAndRead serializes the item's read-modify-write and returns its record
// (zero when absent). The row lock cannot protect an absent row, so an advisory
// transaction lock keyed on the item does.
func lockAndRead(ctx context.Context, tx pgx.Tx, instanceID, key, itemID string) (nowork.Record, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('control.no_work_streak:' || $1 || ':' || $2 || ':' || $3, 0))`,
		instanceID, key, itemID); err != nil {
		return nowork.Record{}, fmt.Errorf("lock no-work streak: %w", err)
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT record FROM control.no_work_streak WHERE instance_id=$1 AND repo_key=$2 AND item_id=$3`,
		instanceID, key, itemID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nowork.Record{}, nil
	}
	if err != nil {
		return nowork.Record{}, err
	}
	var rec nowork.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nowork.Record{}, fmt.Errorf("decode no-work streak: %w", err)
	}
	return rec, nil
}

// Increment advances the item's count by one and records terminal's verdict,
// returning the record it wrote and the one it replaced, both from the same
// serialized step.
func (s Store) Increment(ctx context.Context, handle *lease.Handle, repo providers.RepositoryRef, itemID, runID string, terminal nowork.Terminal) (next, previous nowork.Record, err error) {
	key := blockedcycle.RepositoryIdentity(repo)
	err = s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		if previous, rerr = lockAndRead(ctx, tx, handle.InstanceID, key, itemID); rerr != nil {
			return rerr
		}
		next = nowork.Advance(previous, terminal, runID, s.now())
		encoded, merr := json.Marshal(next)
		if merr != nil {
			return merr
		}
		_, werr := tx.Exec(ctx, `
			INSERT INTO control.no_work_streak (instance_id, repo_key, item_id, record, owner_epoch)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (instance_id, repo_key, item_id) DO UPDATE
			SET record = EXCLUDED.record, owner_epoch = EXCLUDED.owner_epoch, updated_at = now()`,
			handle.InstanceID, key, itemID, encoded, handle.Epoch)
		return werr
	})
	if err != nil {
		return nowork.Record{}, nowork.Record{}, err
	}
	return next, previous, nil
}

// Reset drops the item's record after a productive completion and returns what
// it cleared (zero when there was none), so the caller can flag the verdict the
// run contradicted. A replay finds nothing and clears nothing.
func (s Store) Reset(ctx context.Context, handle *lease.Handle, repo providers.RepositoryRef, itemID, _ string) (nowork.Record, error) {
	key := blockedcycle.RepositoryIdentity(repo)
	var cleared nowork.Record
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		current, err := lockAndRead(ctx, tx, handle.InstanceID, key, itemID)
		if err != nil || current.Count == 0 {
			return err
		}
		cleared = current
		_, err = tx.Exec(ctx, `DELETE FROM control.no_work_streak WHERE instance_id=$1 AND repo_key=$2 AND item_id=$3`,
			handle.InstanceID, key, itemID)
		return err
	})
	return cleared, err
}
