package acceptance

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// reviseChildDisposition applies the same revision contract as the SQLite
// store: an exact repeat of the request that created the current revision is
// idempotent; a replacement attempt that names the current request digest
// supersedes it, archiving the prior choice in bounded history and rebinding
// the disposition to the new attempt; an applied choice is final, and a
// published plan can only be rebound to the same choice.
func (s *Store) reviseChildDisposition(
	ctx context.Context,
	tx pgx.Tx,
	child triggerqueue.ChildRecord,
	prior triggerqueue.ChildDisposition,
	request triggerqueue.ChildDispositionRequest,
	epoch int64,
) (triggerqueue.ChildDisposition, error) {
	sameChoice := prior.Action == request.Action && prior.ResultRef == request.ResultRef
	sameOwner := prior.AttemptID == request.Authority.AttemptID
	// A lost response may repeat exactly the request that created this revision.
	if sameChoice && (sameOwner || !prior.AppliedAt.IsZero()) && request.ExpectedRequestDigest == prior.PreviousDigest {
		return prior, nil
	}
	if request.ExpectedRequestDigest == "" || request.ExpectedRequestDigest != prior.RequestDigest() {
		return triggerqueue.ChildDisposition{}, triggerqueue.ErrConflict
	}
	if sameChoice && sameOwner {
		return prior, nil
	}
	if !prior.AppliedAt.IsZero() || !child.AcknowledgedAt.IsZero() {
		return triggerqueue.ChildDisposition{}, triggerqueue.ErrTransition
	}
	// Rebinding an identical published plan to a replacement attempt retains
	// the exact application; a different action can only replace an
	// unpublished plan.
	if prior.PlanDigest != "" && !sameChoice {
		return triggerqueue.ChildDisposition{}, triggerqueue.ErrChildDispositionReconcile
	}
	if err := s.archiveChildDisposition(ctx, tx, child, prior, epoch); err != nil {
		return triggerqueue.ChildDisposition{}, err
	}
	next := prior
	next.Action, next.AttemptID, next.PreviousDigest = request.Action, request.Authority.AttemptID, prior.RequestDigest()
	if err := tx.QueryRow(ctx, `
		UPDATE control.child_disposition
		SET action=$3, attempt_id=$4, previous_digest=$5,
		    requested_at=GREATEST(clock_timestamp(), requested_at), owner_epoch=$6
		WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$6
		RETURNING requested_at`,
		s.instanceID, child.ChildID, next.Action, next.AttemptID, next.PreviousDigest, epoch,
	).Scan(&next.RequestedAt); err != nil {
		return triggerqueue.ChildDisposition{}, err
	}
	next.RequestedAt = next.RequestedAt.UTC()
	if !next.RequestedAt.After(prior.RequestedAt) {
		// Two revisions in the same instant would share a request digest
		// with their predecessor's time; refuse rather than alias them.
		return triggerqueue.ChildDisposition{}, triggerqueue.ErrTransition
	}
	tag, err := tx.Exec(ctx, `
		UPDATE control.child_disposition SET request_digest=$3
		WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$4`,
		s.instanceID, child.ChildID, next.RequestDigest(), epoch)
	if err := changed(tag.RowsAffected(), err); err != nil {
		return triggerqueue.ChildDisposition{}, err
	}
	return next, nil
}

func (s *Store) archiveChildDisposition(
	ctx context.Context,
	tx pgx.Tx,
	child triggerqueue.ChildRecord,
	prior triggerqueue.ChildDisposition,
	epoch int64,
) error {
	var count int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM control.child_disposition_history
		WHERE instance_id=$1 AND child_id=$2`,
		s.instanceID, child.ChildID).Scan(&count); err != nil {
		return err
	}
	if count >= triggerqueue.MaxChildDispositionRevisions {
		return triggerqueue.ErrChildDispositionHistoryFull
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO control.child_disposition_history (
			instance_id, child_id, revision, request_digest, previous_digest, action,
			result_ref, attempt_id, requested_at, plan_digest, superseded_at, owner_epoch
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,GREATEST(clock_timestamp(), $9),$11)`,
		s.instanceID, child.ChildID, count+1, prior.RequestDigest(), prior.PreviousDigest, prior.Action,
		prior.ResultRef, prior.AttemptID, prior.RequestedAt, prior.PlanDigest, epoch)
	return err
}

// ChildDispositionHistory returns at most MaxChildDispositionRevisions
// immutable prior decisions, oldest first, verifying the digest chain from
// the first request to the current one. Callers own the ownership checks, as
// for ChildDisposition.
func (s *Store) ChildDispositionHistory(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) ([]triggerqueue.ChildDispositionRevision, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil {
		return nil, err
	}
	if !child.TombstonedAt.IsZero() {
		return nil, triggerqueue.ErrTransition
	}
	rows, err := s.pool.Query(ctx, `
		SELECT request_digest, previous_digest, action, result_ref, attempt_id,
		       requested_at, plan_digest, superseded_at
		FROM control.child_disposition_history
		WHERE instance_id=$1 AND child_id=$2
		ORDER BY revision LIMIT $3`,
		s.instanceID, child.ChildID, triggerqueue.MaxChildDispositionRevisions)
	if err != nil {
		return nil, err
	}
	history, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.ChildDispositionRevision, error) {
		var record triggerqueue.ChildDispositionRevision
		var requested, superseded time.Time
		err := row.Scan(&record.RequestDigest, &record.PreviousDigest, &record.Action, &record.ResultRef, &record.AttemptID,
			&requested, &record.PlanDigest, &superseded)
		record.RequestedAt, record.SupersededAt = requested.UTC(), superseded.UTC()
		return record, err
	})
	if err != nil {
		return nil, err
	}
	current, err := s.ChildDisposition(ctx, identity)
	if err != nil {
		return nil, err
	}
	previous := ""
	for _, record := range history {
		entry := triggerqueue.ChildDisposition{Identity: identity, Action: record.Action, ResultRef: record.ResultRef,
			AttemptID: record.AttemptID, RequestedAt: record.RequestedAt, PreviousDigest: record.PreviousDigest}
		if record.RequestDigest != entry.RequestDigest() || record.PreviousDigest != previous || record.SupersededAt.Before(record.RequestedAt) {
			return nil, triggerqueue.ErrConflict
		}
		previous = record.RequestDigest
	}
	if current.PreviousDigest != previous {
		return nil, triggerqueue.ErrConflict
	}
	if history == nil {
		history = []triggerqueue.ChildDispositionRevision{}
	}
	return history, nil
}
