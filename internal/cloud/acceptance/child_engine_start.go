package acceptance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrChildEngineStartOwned refuses a start intent for a child that another
// parent execution, or another frozen manifest, already owns. Phase 1 never
// re-binds an accepted child; restart needs a qualified epoch adapter.
var ErrChildEngineStartOwned = errors.New("cloud acceptance: child start is owned by another parent execution")

// ChildEngineStart is the engine-owned start intent for one accepted child.
type ChildEngineStart struct {
	ManifestDigest      string
	ParentWorkflow      string
	ParentWorkflowID    string
	ParentTemporalRunID string
	Deadline            time.Time
}

func (s ChildEngineStart) valid() bool {
	return blobstore.ValidDigest(s.ManifestDigest) && validText(s.ParentWorkflow, 256, true) &&
		validText(s.ParentWorkflowID, 1024, true) && validText(s.ParentTemporalRunID, 256, true) && !s.Deadline.IsZero()
}

// BindChildEngineStart records the start intent once, in the same fence as
// the child's lineage. An identical retry returns the stored intent, so its
// deadline never moves; any difference is ErrChildEngineStartOwned.
func (s *Store) BindChildEngineStart(ctx context.Context, identity triggerqueue.ChildIdentity, start ChildEngineStart) (ChildEngineStart, error) {
	if !childIdentityValid(identity) || !start.valid() {
		return ChildEngineStart{}, errors.New("cloud acceptance: invalid child engine start")
	}
	var bound ChildEngineStart
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, identity, epoch)
		if err != nil {
			return err
		}
		if !child.TombstonedAt.IsZero() || !child.AcknowledgedAt.IsZero() {
			return triggerqueue.ErrTransition
		}
		existing, err := readChildEngineStart(txCtx, tx, s.instanceID, child.ChildID, true)
		if err == nil {
			start.Deadline = existing.Deadline
			if existing != start {
				return ErrChildEngineStartOwned
			}
			bound = existing
			_, err = tx.Exec(txCtx, `UPDATE control.child_engine_start SET owner_epoch=$3 WHERE instance_id=$1 AND child_id=$2`, s.instanceID, child.ChildID, epoch)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if child.State != triggerqueue.ChildQueued || child.CancellationRequested {
			return triggerqueue.ErrTransition
		}
		_, err = tx.Exec(txCtx, `
			INSERT INTO control.child_engine_start (
				instance_id,child_id,manifest_digest,parent_workflow,parent_workflow_id,
				parent_temporal_run_id,deadline,owner_epoch
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			s.instanceID, child.ChildID, start.ManifestDigest, start.ParentWorkflow, start.ParentWorkflowID,
			start.ParentTemporalRunID, start.Deadline.UTC(), epoch)
		bound = start
		bound.Deadline = start.Deadline.UTC()
		return err
	})
	return bound, err
}

// ChildEngineStart reads one child's engine start intent.
func (s *Store) ChildEngineStart(ctx context.Context, identity triggerqueue.ChildIdentity) (ChildEngineStart, error) {
	if !childIdentityValid(identity) {
		return ChildEngineStart{}, triggerqueue.ErrTransition
	}
	child, err := s.GetChild(ctx, identity)
	if err != nil {
		return ChildEngineStart{}, err
	}
	start, err := readChildEngineStart(ctx, s.pool, s.instanceID, child.ChildID, false)
	return start, noRows(err)
}

func readChildEngineStart(ctx context.Context, db rowQuerier, instanceID, childID string, lock bool) (ChildEngineStart, error) {
	query := `SELECT manifest_digest,parent_workflow,parent_workflow_id,parent_temporal_run_id,deadline
		FROM control.child_engine_start WHERE instance_id=$1 AND child_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	var start ChildEngineStart
	err := db.QueryRow(ctx, query, instanceID, childID).Scan(&start.ManifestDigest, &start.ParentWorkflow, &start.ParentWorkflowID, &start.ParentTemporalRunID, &start.Deadline)
	start.Deadline = start.Deadline.UTC()
	return start, err
}
