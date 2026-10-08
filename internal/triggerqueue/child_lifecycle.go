package triggerqueue

import (
	"context"
	"errors"
	"time"
)

func validChildTransition(from, to ChildState) bool {
	switch from {
	case ChildQueued:
		return to == ChildRunning || to == ChildFailed || to == ChildCancelled
	case ChildRunning:
		return to == ChildAwaitingHuman || to.Terminal()
	case ChildAwaitingHuman:
		return to == ChildRunning || to.Terminal()
	default:
		return false
	}
}

func (u ChildStateUpdate) valid() bool {
	return validChildTransition(u.Expected, u.State) && validChildText(u.ResultRef, MaxChildRefBytes, u.State.Terminal()) && validChildText(u.WorkspaceRef, MaxChildRefBytes, false) && (u.State.Terminal() || u.ResultRef == "")
}

// SetChildState records an observed execution outcome, never an instruction to
// execute or cancel a child. The caller must first persist/verify ResultRef.
// Terminal outcomes are immutable and retain the occurrence slot until ack.
// A repeated identical transition is safe after a lost response.
func (s *Store) SetChildState(ctx context.Context, identity ChildIdentity, update ChildStateUpdate, now time.Time) error {
	if !identity.valid() || now.IsZero() || !update.valid() {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(identity)...))
	if err != nil {
		return err
	}
	if !c.TombstonedAt.IsZero() {
		return ErrTransition
	}
	if c.State == update.State && c.ResultRef == update.ResultRef && c.WorkspaceRef == update.WorkspaceRef {
		return nil
	}
	if c.State != update.Expected || now.Before(c.UpdatedAt) {
		return ErrTransition
	}
	var receiptState State
	if err = tx.QueryRowContext(ctx, `SELECT state FROM triggers WHERE id=?`, c.AcceptanceID).Scan(&receiptState); err != nil {
		return err
	}
	// A queued start has not executed. A terminal observation cannot race its
	// dispatch into starting afterward; both changes are committed together.
	if c.State == ChildQueued && update.State.Terminal() && receiptState == Accepted {
		if _, err = tx.ExecContext(ctx, `UPDATE triggers SET state='rejected',reason=?,finished_ns=? WHERE id=? AND state='accepted'`, "child settled before dispatch", now.UnixNano(), c.AcceptanceID); err != nil {
			return err
		}
	}
	if update.State == ChildRunning && receiptState != Dispatching && receiptState != Dispatched {
		return ErrTransition
	}
	var terminal any
	if update.State.Terminal() {
		terminal = now.UnixNano()
	}
	args := []any{update.State, update.ResultRef, update.WorkspaceRef, now.UnixNano(), terminal}
	args = append(args, childArgs(identity)...)
	args = append(args, update.Expected)
	res, err := tx.ExecContext(ctx, `UPDATE child_lineages SET state=?,result_ref=?,workspace_ref=?,updated_ns=?,terminal_ns=? WHERE gaggle=? AND parent_run=? AND occurrence=? AND invocation_key=? AND state=?`, args...)
	if err = changed(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

// AcknowledgeChild releases the unresolved-child slot only after the parent
// durably consumed this exact terminal result. Failed and cancelled children
// must also be acknowledged. An acknowledgement is not permission to retry.
func (s *Store) AcknowledgeChild(ctx context.Context, identity ChildIdentity, expectedResultRef string, now time.Time) error {
	if !identity.valid() || !validChildText(expectedResultRef, MaxChildRefBytes, true) || now.IsZero() {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(identity)...))
	if err != nil {
		return err
	}
	if !c.TombstonedAt.IsZero() || !c.State.Terminal() || c.ResultRef != expectedResultRef {
		return ErrTransition
	}
	if !c.AcknowledgedAt.IsZero() {
		return nil
	}
	if now.Before(c.UpdatedAt) {
		return ErrTransition
	}
	args := append([]any{now.UnixNano(), now.UnixNano()}, childArgs(identity)...)
	res, err := tx.ExecContext(ctx, `UPDATE child_lineages SET acknowledged_ns=?,updated_ns=? WHERE gaggle=? AND parent_run=? AND occurrence=? AND invocation_key=? AND acknowledged_ns IS NULL`, args...)
	if err = changed(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

// FenceChildParent atomically prevents new children and new dispatch claims.
// Already claimed executions still require cancellation and reconciliation;
// PendingChildCancellations is the durable, repeatable cancellation outbox.
// This does not claim that the parent or any running child has stopped.
func (s *Store) FenceChildParent(ctx context.Context, parent ChildParent, actor string, now time.Time) error {
	if !parent.valid() || !validChildText(actor, 1024, true) || now.IsZero() {
		return errors.New("triggerqueue: invalid child parent fence")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = ensureChildParent(ctx, tx, parent, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_parents SET cancelled_ns=?,cancel_actor=? WHERE gaggle=? AND parent_run=? AND cancelled_ns IS NULL`, now.UnixNano(), actor, parent.Gaggle, parent.ParentRunID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE triggers SET state='rejected',reason='parent cancellation requested',finished_ns=? WHERE state='accepted' AND id IN (SELECT acceptance_id FROM child_lineages WHERE gaggle=? AND parent_run=?)`, now.UnixNano(), parent.Gaggle, parent.ParentRunID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkChildParentSettled closes submission and starts the family retention
// clock. Call only after the parent is durably terminal, not for a human wait.
// Unacknowledged family members remain pinned even after this call.
func (s *Store) MarkChildParentSettled(ctx context.Context, parent ChildParent, now time.Time) error {
	if !parent.valid() || now.IsZero() {
		return errors.New("triggerqueue: invalid child parent settlement")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = ensureChildParent(ctx, tx, parent, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_parents SET settled_ns=? WHERE gaggle=? AND parent_run=? AND settled_ns IS NULL`, now.UnixNano(), parent.Gaggle, parent.ParentRunID); err != nil {
		return err
	}
	return tx.Commit()
}

// Children pages one parent's lineage with an opaque ChildID cursor. This is
// an internal recovery lookup, not an authorization boundary.
func (s *Store) Children(ctx context.Context, parent ChildParent, after string, limit int) ([]ChildRecord, error) {
	return s.childPage(ctx, parent, after, limit, false)
}

// PendingChildCancellations pages unfinished children after a parent fence.
// Repeating cancellation after a crash is intentional; terminal observation,
// rather than delivery of a cancel request, removes an entry from this outbox.
func (s *Store) PendingChildCancellations(ctx context.Context, parent ChildParent, after string, limit int) ([]ChildRecord, error) {
	return s.childPage(ctx, parent, after, limit, true)
}

func (s *Store) childPage(ctx context.Context, parent ChildParent, after string, limit int, cancellations bool) ([]ChildRecord, error) {
	if !parent.valid() || len(after) > 128 || limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid child page")
	}
	filter := ""
	if cancellations {
		filter = ` AND p.cancelled_ns IS NOT NULL AND c.state IN ('queued','running','awaiting_human')`
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+childColumns+childFrom+` WHERE c.gaggle=? AND c.parent_run=? AND c.child_id>?`+filter+` ORDER BY c.child_id LIMIT ?`, parent.Gaggle, parent.ParentRunID, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []ChildRecord
	for rows.Next() {
		c, err := scanChild(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, c)
	}
	return records, rows.Err()
}
