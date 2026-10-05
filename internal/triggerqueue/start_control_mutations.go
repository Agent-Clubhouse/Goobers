package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func (c StartCancellation) valid() bool {
	return validChildText(c.RequestID, 128, true) && validChildText(c.Actor, 1024, true) && validChildText(c.Reason, 512, true) && len(c.Authority) > 0 && len(c.Authority) <= 8192 && json.Valid(c.Authority)
}

// RequestStartCancellation durably records the first exact authorized command.
// Only accepted/no-effect custody can settle immediately. Attempted execution
// remains retained and requires host reconciliation/cancellation observation.
func (s *Store) RequestStartCancellation(ctx context.Context, gaggle, id string, command StartCancellation, now time.Time) (StartControl, bool, error) {
	if !command.valid() || now.IsZero() {
		return StartControl{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StartControl{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := readStartControl(ctx, tx, gaggle, id)
	if err != nil {
		return c, false, err
	}
	raw, _ := json.Marshal(command)
	if c.Cancellation != nil {
		prior, _ := json.Marshal(c.Cancellation)
		if string(raw) != string(prior) {
			return c, false, ErrConflict
		}
		return c, true, nil
	}
	if c.Record.State == Rejected || now.Before(c.Record.AcceptedAt) {
		return c, false, ErrTransition
	}
	if c.Record.State == Accepted {
		if err = settleUnstartedControlledSource(ctx, tx, c, "cancelled", now); err != nil {
			return c, false, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE start_controls SET cancellation=?,cancel_ns=?,reserved_bytes=MAX(0,reserved_bytes-?) WHERE acceptance_id=? AND length(cancellation)=0`, raw, now.UnixNano(), len(raw)+1024, id)
	if err != nil {
		return c, false, err
	}
	c, err = readStartControl(ctx, tx, gaggle, id)
	if err != nil {
		return c, false, err
	}
	return c, false, tx.Commit()
}

// ExpireStartControl applies a captured deadline only while custody is accepted.
// A claimed or uncertain effect can never be expired by elapsed time.
func (s *Store) ExpireStartControl(ctx context.Context, gaggle, id string, now time.Time) (StartControl, bool, error) {
	if now.IsZero() {
		return StartControl{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StartControl{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := readStartControl(ctx, tx, gaggle, id)
	if err != nil {
		return c, false, err
	}
	if c.Record.State != Accepted || c.Scope.Deadline.IsZero() || now.Before(c.Scope.Deadline) {
		return c, false, nil
	}
	if err = settleUnstartedControlledSource(ctx, tx, c, "expired", now); err != nil {
		return c, false, err
	}
	c, err = readStartControl(ctx, tx, gaggle, id)
	if err != nil {
		return c, false, err
	}
	return c, true, tx.Commit()
}

func settleControlledReceipt(ctx context.Context, tx *sql.Tx, c StartControl, disposition string, now time.Time) error {
	if c.Record.State != Accepted || (disposition != "cancelled" && disposition != "expired") {
		return ErrTransition
	}
	res, err := tx.ExecContext(ctx, `UPDATE triggers SET state='rejected',reason=?,finished_ns=? WHERE id=? AND state='accepted' AND finished_ns IS NULL`, disposition+" before dispatch", now.UnixNano(), c.Record.ID)
	if err = changed(res, err); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE start_controls SET disposition=?,disposed_ns=? WHERE acceptance_id=?`, disposition, now.UnixNano(), c.Record.ID)
	return err
}

// ReconcileStartCancellation settles a requested cancellation after a qualified
// no-effect refusal or startup absence proof returned custody to accepted. It
// cannot turn uncertain/attempted custody into proof that execution stopped.
func (s *Store) ReconcileStartCancellation(ctx context.Context, gaggle, id string, now time.Time) (StartControl, bool, error) {
	if now.IsZero() {
		return StartControl{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StartControl{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := readStartControl(ctx, tx, gaggle, id)
	if err != nil {
		return c, false, err
	}
	if c.Cancellation == nil || c.Record.State != Accepted {
		return c, false, nil
	}
	if now.Before(c.CancelRequestedAt) {
		return c, false, ErrTransition
	}
	if err = settleUnstartedControlledSource(ctx, tx, c, "cancelled", now); err != nil {
		return c, false, err
	}
	c, err = readStartControl(ctx, tx, gaggle, id)
	if err != nil {
		return c, false, err
	}
	return c, true, tx.Commit()
}
