// Package terminalwatermarkpg stores the terminal awaiter's closed-run
// watermark in PostgreSQL, so a successor leader can find the runs that closed
// while no leader was awaiting them.
package terminalwatermarkpg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// Store is the watermark for one instance.
type Store struct {
	Pool  *pgxpool.Pool
	Fence *lease.Manager
}

// Load returns the watermark, and false when none has been recorded.
func (s Store) Load(ctx context.Context, instanceID string) (time.Time, bool, error) {
	var through time.Time
	err := s.Pool.QueryRow(ctx,
		`SELECT closed_through FROM control.terminal_watermark WHERE instance_id=$1`, instanceID).Scan(&through)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return through, err == nil, err
}

// Advance moves the watermark forward to through under the controller-lease
// fence; a stale or self-fenced leader fails, and an earlier value is a no-op.
func (s Store) Advance(ctx context.Context, handle *lease.Handle, through time.Time) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO control.terminal_watermark (instance_id, closed_through, owner_epoch)
			VALUES ($1, $2, $3)
			ON CONFLICT (instance_id) DO UPDATE
			SET closed_through = GREATEST(control.terminal_watermark.closed_through, EXCLUDED.closed_through),
			    owner_epoch = EXCLUDED.owner_epoch, updated_at = now()`,
			handle.InstanceID, through, handle.Epoch)
		return err
	})
}
