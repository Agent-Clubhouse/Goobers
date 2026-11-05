package acceptance

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// Prune removes one bounded batch of expired receipts under the live controller
// fence. API acceptance only inserts/reads; it never needs DELETE authority.
func (s *Store) Prune(ctx context.Context) error {
	return s.withFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		_, err := tx.Exec(ctx, `DELETE FROM control.acceptance WHERE instance_id=$1 AND id IN (
    SELECT id FROM control.acceptance AS a WHERE instance_id=$1 AND finished_at IS NOT NULL
    AND accepted_at<clock_timestamp()-$2*interval '1 second' AND owner_epoch>0 AND owner_epoch<=$3
    AND NOT EXISTS (SELECT 1 FROM control.child_lineage AS c
      WHERE c.instance_id=a.instance_id AND c.acceptance_id=a.id)
    ORDER BY accepted_at,id FOR UPDATE SKIP LOCKED LIMIT 256)`, s.instanceID, triggerqueue.ReplayRetention.Seconds(), epoch)
		return err
	})
}
