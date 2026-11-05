package acceptance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// SetChildState applies a monotonic child execution CAS using the database
// clock. Identical lost-response retries are accepted.
func (s *Store) SetChildState(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	update triggerqueue.ChildStateUpdate,
	now time.Time,
) error {
	if !childIdentityValid(identity) || now.IsZero() ||
		!validChildTransition(update.Expected, update.State) ||
		!validText(update.ResultRef, triggerqueue.MaxChildRefBytes, update.State.Terminal()) ||
		!validText(update.WorkspaceRef, triggerqueue.MaxChildRefBytes, false) ||
		(!update.State.Terminal() && update.ResultRef != "") {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		current, err := s.lockChild(txCtx, tx, identity, epoch)
		if err != nil {
			return err
		}
		if !current.TombstonedAt.IsZero() {
			return triggerqueue.ErrTransition
		}
		if current.State == update.State &&
			current.ResultRef == update.ResultRef &&
			current.WorkspaceRef == update.WorkspaceRef {
			return nil
		}
		if current.State != update.Expected {
			return triggerqueue.ErrTransition
		}
		var acceptanceState triggerqueue.State
		if err := tx.QueryRow(txCtx, `
			SELECT state FROM control.acceptance
			WHERE instance_id=$1 AND id=$2 FOR UPDATE`,
			s.instanceID, current.AcceptanceID,
		).Scan(&acceptanceState); err != nil {
			return err
		}
		if update.State == triggerqueue.ChildRunning &&
			acceptanceState != triggerqueue.Dispatching &&
			acceptanceState != triggerqueue.Dispatched {
			return triggerqueue.ErrTransition
		}
		if current.State == triggerqueue.ChildQueued &&
			update.State.Terminal() && acceptanceState == triggerqueue.Accepted {
			if _, err := tx.Exec(txCtx, `
				UPDATE control.acceptance
				SET state='rejected',reason='child settled before dispatch',
				    finished_at=clock_timestamp()
				WHERE instance_id=$1 AND id=$2 AND state='accepted'
				  AND owner_epoch=$3`,
				s.instanceID, current.AcceptanceID, epoch,
			); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_lineage
			SET state=$6,result_ref=$7,workspace_ref=$8,
			    updated_at=clock_timestamp(),
			    terminal_at=CASE WHEN $9 THEN clock_timestamp() ELSE NULL END,
			    owner_epoch=$10
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			  AND occurrence=$4 AND invocation_key=$5
			  AND state=$11 AND owner_epoch=$10`,
			s.instanceID, identity.Gaggle, identity.ParentRunID,
			identity.StageOccurrence, identity.InvocationKey,
			update.State, update.ResultRef, update.WorkspaceRef,
			update.State.Terminal(), epoch, update.Expected,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// AcknowledgeChild releases the occurrence slot only for the exact terminal
// result consumed by the parent.
func (s *Store) AcknowledgeChild(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	expectedResultRef string,
	now time.Time,
) error {
	if !childIdentityValid(identity) ||
		!validText(expectedResultRef, triggerqueue.MaxChildRefBytes, true) ||
		now.IsZero() {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		current, err := s.lockChild(txCtx, tx, identity, epoch)
		if err != nil {
			return err
		}
		if !current.TombstonedAt.IsZero() || !current.State.Terminal() ||
			current.ResultRef != expectedResultRef {
			return triggerqueue.ErrTransition
		}
		if !current.AcknowledgedAt.IsZero() {
			return nil
		}
		var disposition bool
		if err := tx.QueryRow(txCtx, `
			SELECT EXISTS (
				SELECT 1 FROM control.child_disposition
				WHERE instance_id=$1 AND child_id=$2
			)`,
			s.instanceID, current.ChildID,
		).Scan(&disposition); err != nil {
			return err
		}
		if disposition {
			return triggerqueue.ErrTransition
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_lineage
			SET acknowledged_at=clock_timestamp(),updated_at=clock_timestamp(),
			    owner_epoch=$6
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			  AND occurrence=$4 AND invocation_key=$5
			  AND acknowledged_at IS NULL AND owner_epoch=$6`,
			s.instanceID, identity.Gaggle, identity.ParentRunID,
			identity.StageOccurrence, identity.InvocationKey, epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// FenceChildParent durably prevents new admissions and rejects queued starts.
func (s *Store) FenceChildParent(
	ctx context.Context,
	parent triggerqueue.ChildParent,
	actor string,
	now time.Time,
) error {
	if !childParentValid(parent) || !validText(actor, 1024, true) || now.IsZero() {
		return errors.New("cloud acceptance: invalid child parent fence")
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		if err := s.ensureChildParent(txCtx, tx, parent, epoch); err != nil {
			return err
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_parent
			SET cancelled_at=COALESCE(cancelled_at,clock_timestamp()),
			    cancel_actor=CASE WHEN cancelled_at IS NULL THEN $4 ELSE cancel_actor END,
			    owner_epoch=$5
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			  AND owner_epoch=$5`,
			s.instanceID, parent.Gaggle, parent.ParentRunID, actor, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		_, err = tx.Exec(txCtx, `
			UPDATE control.acceptance AS a
			SET state='rejected',reason='parent cancellation requested',
			    finished_at=clock_timestamp()
			WHERE a.instance_id=$1 AND a.state='accepted' AND a.owner_epoch=$4
			  AND EXISTS (
				SELECT 1 FROM control.child_lineage AS c
				WHERE c.instance_id=a.instance_id AND c.acceptance_id=a.id
				  AND c.gaggle=$2 AND c.parent_run_id=$3 AND c.owner_epoch=$4
			  )`,
			s.instanceID, parent.Gaggle, parent.ParentRunID, epoch,
		)
		return err
	})
}

// MarkChildParentSettled closes later child submission and starts retention.
func (s *Store) MarkChildParentSettled(
	ctx context.Context,
	parent triggerqueue.ChildParent,
	now time.Time,
) error {
	if !childParentValid(parent) || now.IsZero() {
		return errors.New("cloud acceptance: invalid child parent settlement")
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		if err := s.ensureChildParent(txCtx, tx, parent, epoch); err != nil {
			return err
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_parent
			SET settled_at=COALESCE(settled_at,clock_timestamp()),owner_epoch=$4
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			  AND owner_epoch=$4`,
			s.instanceID, parent.Gaggle, parent.ParentRunID, epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// Children returns one bounded parent lineage page.
func (s *Store) Children(
	ctx context.Context,
	parent triggerqueue.ChildParent,
	after string,
	limit int,
) ([]triggerqueue.ChildRecord, error) {
	return s.childPage(ctx, parent, after, limit, false)
}

// PendingChildCancellations returns unfinished children after a family fence.
func (s *Store) PendingChildCancellations(
	ctx context.Context,
	parent triggerqueue.ChildParent,
	after string,
	limit int,
) ([]triggerqueue.ChildRecord, error) {
	return s.childPage(ctx, parent, after, limit, true)
}

// ChildCancellationTime reads the first durable family cancellation.
func (s *Store) ChildCancellationTime(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) (time.Time, error) {
	if !childIdentityValid(identity) {
		return time.Time{}, errors.New("cloud acceptance: invalid child cancellation identity")
	}
	var cancelled time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT p.cancelled_at
		FROM control.child_lineage AS c
		JOIN control.child_parent AS p
		  ON p.instance_id=c.instance_id AND p.gaggle=c.gaggle
		 AND p.parent_run_id=c.parent_run_id
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.occurrence=$4 AND c.invocation_key=$5
		  AND c.tombstoned_at IS NULL AND p.cancelled_at IS NOT NULL`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
	).Scan(&cancelled)
	return cancelled.UTC(), noRows(err)
}

func (s *Store) childPage(
	ctx context.Context,
	parent triggerqueue.ChildParent,
	after string,
	limit int,
	cancellations bool,
) ([]triggerqueue.ChildRecord, error) {
	if !childParentValid(parent) || len(after) > 128 || limit < 1 || limit > 100 {
		return nil, errors.New("cloud acceptance: invalid child page")
	}
	filter := ""
	if cancellations {
		filter = ` AND p.cancelled_at IS NOT NULL
			AND c.state IN ('queued','running','awaiting_human')`
	}
	rows, err := s.pool.Query(ctx, childColumns+`
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.child_id>$4`+filter+`
		ORDER BY c.child_id LIMIT $5`,
		s.instanceID, parent.Gaggle, parent.ParentRunID, after, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.ChildRecord, error) {
		return s.scanChild(row)
	})
}

func (s *Store) lockChild(
	ctx context.Context,
	tx pgx.Tx,
	identity triggerqueue.ChildIdentity,
	epoch int64,
) (triggerqueue.ChildRecord, error) {
	record, err := s.scanChild(tx.QueryRow(ctx,
		childColumns+`
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.occurrence=$4 AND c.invocation_key=$5
		  AND c.owner_epoch=$6
		FOR UPDATE`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey, epoch,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return record, triggerqueue.ErrTransition
	}
	return record, err
}
