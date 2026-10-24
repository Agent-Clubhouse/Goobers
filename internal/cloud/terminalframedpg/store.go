// Package terminalframedpg stores the per-run "terminal frame completed"
// receipts, so a successor leader skips runs whose hooks already fired.
package terminalframedpg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// Store is the receipt table for one instance.
type Store struct {
	Pool  *pgxpool.Pool
	Fence *lease.Manager
}

// Framed reports whether a run's frame completed under any leader.
func (s Store) Framed(ctx context.Context, instanceID, runID string) (bool, error) {
	var found bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM control.terminal_framed WHERE instance_id=$1 AND run_id=$2)`,
		instanceID, runID).Scan(&found)
	return found, err
}

// MarkFramed records the receipt under the controller-lease fence; recording it
// twice is a no-op.
func (s Store) MarkFramed(ctx context.Context, handle *lease.Handle, runID string) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO control.terminal_framed (instance_id, run_id, owner_epoch) VALUES ($1, $2, $3)
			ON CONFLICT (instance_id, run_id) DO NOTHING`, handle.InstanceID, runID, handle.Epoch)
		return err
	})
}

// Prune deletes receipts framed before cutoff and returns how many.
func (s Store) Prune(ctx context.Context, handle *lease.Handle, cutoff time.Time) (int64, error) {
	var n int64
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM control.terminal_framed WHERE instance_id=$1 AND framed_at < $2`,
			handle.InstanceID, cutoff)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}
