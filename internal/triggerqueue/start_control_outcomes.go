package triggerqueue

import (
	"context"
	"time"
)

const startControlOutcomeSchema = `ALTER TABLE start_controls ADD COLUMN cancel_outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE start_controls ADD COLUMN cancel_observed_ns INTEGER;`

// CompleteStartCancellation records qualified host evidence only. It never
// changes dispatch state, invents a run, or turns a request into a resend grant.
func (s *Store) CompleteStartCancellation(ctx context.Context, gaggle, id, requestID, outcome string, now time.Time) (StartControl, error) {
	if outcome != "confirmed" && outcome != "already-terminal" || now.IsZero() {
		return StartControl{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StartControl{}, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := readStartControl(ctx, tx, gaggle, id)
	if err != nil {
		return c, err
	}
	if c.Cancellation == nil || c.Cancellation.RequestID != requestID || now.Before(c.CancelRequestedAt) || c.Disposition != "" {
		return c, ErrTransition
	}
	if c.CancellationOutcome != "" {
		if c.CancellationOutcome != outcome {
			return c, ErrConflict
		}
		return c, nil
	}
	if c.Record.State != Dispatching && c.Record.State != Dispatched && c.Record.State != Rejected {
		return c, ErrTransition
	}
	_, err = tx.ExecContext(ctx, `UPDATE start_controls SET cancel_outcome=?,cancel_observed_ns=? WHERE acceptance_id=? AND cancel_outcome=''`, outcome, now.UnixNano(), id)
	if err != nil {
		return c, err
	}
	// Keep completed command evidence for the ordinary replay window even when
	// execution was admitted long before its cancellation was observed.
	if _, err = tx.ExecContext(ctx, `UPDATE triggers SET finished_ns=MAX(finished_ns,?) WHERE id=? AND state IN ('dispatched','rejected')`, now.UnixNano(), id); err != nil {
		return c, err
	}
	c, err = readStartControl(ctx, tx, gaggle, id)
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}
