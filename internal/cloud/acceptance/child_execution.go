package acceptance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// ActiveChildren returns bounded retained execution custody.
func (s *Store) ActiveChildren(
	ctx context.Context,
	after string,
	limit int,
) ([]triggerqueue.ChildRecord, error) {
	if len(after) > 128 || limit < 1 || limit > 100 {
		return nil, errors.New("cloud acceptance: invalid active child page")
	}
	rows, err := s.pool.Query(ctx, childColumns+`
		WHERE c.instance_id=$1 AND c.child_id>$2
		  AND c.tombstoned_at IS NULL
		  AND c.state IN ('queued','running','awaiting_human')
		ORDER BY c.child_id LIMIT $3`,
		s.instanceID, after, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.ChildRecord, error) {
		return s.scanChild(row)
	})
}

// ChildStart returns the original acceptance receipt for one exact lineage.
func (s *Store) ChildStart(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) (triggerqueue.Record, error) {
	if !childIdentityValid(identity) {
		return triggerqueue.Record{}, triggerqueue.ErrTransition
	}
	record, err := scanRecord(s.pool.QueryRow(ctx, acceptanceColumns+`
		FROM control.acceptance AS a
		WHERE a.instance_id=$1 AND a.id=(
			SELECT acceptance_id FROM control.child_lineage
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			  AND occurrence=$4 AND invocation_key=$5 AND tombstoned_at IS NULL
		)`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
	))
	return record, noRows(err)
}

// VerifiedChildStart verifies immutable accepted actor and payload custody.
func (s *Store) VerifiedChildStart(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	actor string,
) (triggerqueue.Record, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil || !child.TombstonedAt.IsZero() {
		return triggerqueue.Record{}, triggerqueue.ErrTransition
	}
	receipt, err := s.Get(ctx, child.AcceptanceID, actor)
	if err != nil {
		return triggerqueue.Record{}, err
	}
	var actorDigest, payloadDigest string
	if err := s.pool.QueryRow(ctx, `
		SELECT actor_digest,payload_digest FROM control.child_lineage
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
		  AND occurrence=$4 AND invocation_key=$5`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
	).Scan(&actorDigest, &payloadDigest); err != nil {
		return triggerqueue.Record{}, err
	}
	if actorDigest != childDigest([]byte(actor)) ||
		payloadDigest != childDigest(receipt.Payload) ||
		receipt.Key != child.StartKey {
		return triggerqueue.Record{}, triggerqueue.ErrConflict
	}
	return receipt, nil
}

// ChildForExecutionRun resolves custody by the immutable accepted run ID.
func (s *Store) ChildForExecutionRun(
	ctx context.Context,
	runID string,
) (triggerqueue.ChildRecord, error) {
	record, err := s.scanChild(s.pool.QueryRow(ctx, childColumns+`
		WHERE c.instance_id=$1 AND c.child_id=$2`,
		s.instanceID, "child-"+runID,
	))
	return record, noRows(err)
}

// CurrentChild returns the sole unresolved owner of an occurrence.
func (s *Store) CurrentChild(
	ctx context.Context,
	parent triggerqueue.ChildParent,
	occurrence string,
) (triggerqueue.ChildRecord, error) {
	if !childParentValid(parent) || !validText(occurrence, 256, true) {
		return triggerqueue.ChildRecord{}, triggerqueue.ErrTransition
	}
	rows, err := s.pool.Query(ctx, childColumns+`
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.occurrence=$4 AND c.acknowledged_at IS NULL
		  AND c.tombstoned_at IS NULL
		ORDER BY c.sequence LIMIT 2`,
		s.instanceID, parent.Gaggle, parent.ParentRunID, occurrence,
	)
	if err != nil {
		return triggerqueue.ChildRecord{}, err
	}
	defer rows.Close()
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.ChildRecord, error) {
		return s.scanChild(row)
	})
	if err != nil {
		return triggerqueue.ChildRecord{}, err
	}
	if len(records) == 0 {
		return triggerqueue.ChildRecord{}, sql.ErrNoRows
	}
	if len(records) > 1 {
		return triggerqueue.ChildRecord{}, fmt.Errorf(
			"cloud acceptance: child occurrence has multiple unresolved owners")
	}
	return records[0], nil
}

// RequeueChild resolves a proven absent launch without crossing a parent fence.
func (s *Store) RequeueChild(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	reason string,
	now time.Time,
) error {
	if !childIdentityValid(identity) || len(reason) > 1024 || now.IsZero() {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, identity, epoch)
		if err != nil {
			return err
		}
		state := triggerqueue.Accepted
		finished := false
		if child.CancellationRequested {
			state, reason, finished = triggerqueue.Rejected, "parent cancellation requested", true
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.acceptance
			SET state=$4,reason=$5,
			    finished_at=CASE WHEN $6 THEN clock_timestamp() ELSE NULL END,
			    owner_epoch=CASE WHEN $6 THEN $3 ELSE NULL END
			WHERE instance_id=$1 AND id=$2 AND state='dispatching'
			  AND owner_epoch=$3 AND finished_at IS NULL`,
			s.instanceID, child.AcceptanceID, epoch, state, reason, finished,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// ChildRejection returns the durable rejected-start observation.
func (s *Store) ChildRejection(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) (time.Time, string, error) {
	var at time.Time
	var reason string
	err := s.pool.QueryRow(ctx, `
		SELECT a.finished_at,a.reason
		FROM control.acceptance AS a
		JOIN control.child_lineage AS c
		  ON c.instance_id=a.instance_id AND c.acceptance_id=a.id
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.occurrence=$4 AND c.invocation_key=$5
		  AND a.state='rejected' AND a.finished_at IS NOT NULL`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
	).Scan(&at, &reason)
	return at.UTC(), reason, noRows(err)
}

// WithChildLaunch holds the family and lineage fence around bounded publication.
func (s *Store) WithChildLaunch(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	publish func() error,
) error {
	if !childIdentityValid(identity) || publish == nil {
		return errors.New("cloud acceptance: invalid child launch barrier")
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, identity, epoch)
		if err != nil {
			return err
		}
		if child.State != triggerqueue.ChildQueued ||
			child.CancellationRequested || !child.TombstonedAt.IsZero() {
			return triggerqueue.ErrTransition
		}
		var state triggerqueue.State
		if err := tx.QueryRow(txCtx, `
			SELECT state FROM control.acceptance
			WHERE instance_id=$1 AND id=$2 AND owner_epoch=$3 FOR UPDATE`,
			s.instanceID, child.AcceptanceID, epoch,
		).Scan(&state); err != nil {
			return err
		}
		if state != triggerqueue.Dispatching {
			return triggerqueue.ErrTransition
		}
		return publish()
	})
}

// WithChildResume holds the family and execution fence around recovery readiness.
func (s *Store) WithChildResume(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	runID string,
	ready func() error,
) error {
	if !childIdentityValid(identity) || !validText(runID, 256, true) || ready == nil {
		return errors.New("cloud acceptance: invalid child resume barrier")
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, identity, epoch)
		if err != nil {
			return err
		}
		if child.RunID != runID ||
			(child.State != triggerqueue.ChildQueued && child.State != triggerqueue.ChildRunning) ||
			child.CancellationRequested || !child.TombstonedAt.IsZero() {
			return triggerqueue.ErrTransition
		}
		var state triggerqueue.State
		if err := tx.QueryRow(txCtx, `
			SELECT state FROM control.acceptance
			WHERE instance_id=$1 AND id=$2 AND owner_epoch=$3 FOR UPDATE`,
			s.instanceID, child.AcceptanceID, epoch,
		).Scan(&state); err != nil {
			return err
		}
		if state != triggerqueue.Dispatching && state != triggerqueue.Dispatched {
			return triggerqueue.ErrTransition
		}
		return ready()
	})
}

// IsChildParent reports whether bounded family custody exists.
func (s *Store) IsChildParent(
	ctx context.Context,
	parent triggerqueue.ChildParent,
) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM control.child_parent
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
		)`,
		s.instanceID, parent.Gaggle, parent.ParentRunID,
	).Scan(&exists)
	return exists, err
}

// RetainedChildStarts enumerates full source-custody owners.
func (s *Store) RetainedChildStarts(
	ctx context.Context,
	after string,
	limit int,
) ([]triggerqueue.Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("cloud acceptance: batch limit must be 1..100")
	}
	rows, err := s.pool.Query(ctx, acceptanceColumns+`
		FROM control.acceptance AS a
		JOIN control.child_lineage AS c
		  ON c.instance_id=a.instance_id AND c.acceptance_id=a.id
		WHERE c.instance_id=$1 AND c.tombstoned_at IS NULL AND a.id>$2
		ORDER BY a.id LIMIT $3`,
		s.instanceID, after, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.Record, error) {
		return scanRecord(row)
	})
}

// UnsettledChildParents includes authority-only and admitted open families.
func (s *Store) UnsettledChildParents(
	ctx context.Context,
	after triggerqueue.ChildParent,
	limit int,
) ([]triggerqueue.ChildParent, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("cloud acceptance: batch limit must be 1..100")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT gaggle,parent_run_id FROM control.child_parent
		WHERE instance_id=$1 AND settled_at IS NULL
		  AND (gaggle>$2 OR (gaggle=$2 AND parent_run_id>$3))
		ORDER BY gaggle,parent_run_id LIMIT $4`,
		s.instanceID, after.Gaggle, after.ParentRunID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.ChildParent, error) {
		var parent triggerqueue.ChildParent
		err := row.Scan(&parent.Gaggle, &parent.ParentRunID)
		return parent, err
	})
}

// RetainsRunJournal reports live parent or child journal dependencies.
func (s *Store) RetainsRunJournal(
	ctx context.Context,
	gaggle string,
	runID string,
) (bool, error) {
	var retained bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM control.child_parent
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND settled_at IS NULL
			UNION ALL
			SELECT 1 FROM control.child_lineage
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND tombstoned_at IS NULL
			UNION ALL
			SELECT 1 FROM control.child_lineage
			WHERE instance_id=$1 AND gaggle=$2 AND child_id=$4 AND tombstoned_at IS NULL
		)`,
		s.instanceID, gaggle, runID, "child-"+runID,
	).Scan(&retained)
	return retained, err
}
