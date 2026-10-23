package acceptance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// PruneChildren bounds one DB-clock retention pass. Object bytes remain under
// the object store's independent soft-delete and GC policy after pointers drop.
func (s *Store) PruneChildren(
	ctx context.Context,
	now time.Time,
	limit int,
) (result triggerqueue.ChildPruneResult, err error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return result, errors.New("cloud acceptance: invalid child prune batch")
	}
	err = s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		remaining := limit
		rows, err := tx.Query(txCtx, `
			SELECT c.child_id,c.acceptance_id
			FROM control.child_lineage AS c
			JOIN control.child_parent AS p
			  ON p.instance_id=c.instance_id AND p.gaggle=c.gaggle
			 AND p.parent_run_id=c.parent_run_id
			LEFT JOIN control.acceptance AS a
			  ON a.instance_id=c.instance_id AND a.id=c.acceptance_id
			WHERE c.instance_id=$1 AND c.owner_epoch=$2
			  AND c.tombstoned_at IS NULL AND c.acknowledged_at IS NOT NULL
			  AND p.settled_at <= clock_timestamp()-$3::interval
			  AND COALESCE(a.state,'rejected')<>'dispatching'
			  AND NOT EXISTS (
				SELECT 1 FROM control.child_lineage AS sibling
				WHERE sibling.instance_id=c.instance_id
				  AND sibling.gaggle=c.gaggle
				  AND sibling.parent_run_id=c.parent_run_id
				  AND sibling.acknowledged_at IS NULL
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM control.child_lineage AS descendant
				WHERE descendant.instance_id=c.instance_id
				  AND descendant.gaggle=c.gaggle
				  AND descendant.parent_run_id=a.run_id
				  AND descendant.tombstoned_at IS NULL
			  )
			ORDER BY c.child_id
			FOR UPDATE OF c SKIP LOCKED LIMIT $4`,
			s.instanceID, epoch, triggerqueue.ChildRetention, remaining,
		)
		if err != nil {
			return err
		}
		type candidate struct{ childID, acceptanceID string }
		candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
			var value candidate
			err := row.Scan(&value.childID, &value.acceptanceID)
			return value, err
		})
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			tag, err := tx.Exec(txCtx, `
				UPDATE control.child_lineage
				SET tombstoned_at=clock_timestamp(),result_ref='',workspace_ref='',
				    snapshot_receipt_digest='',snapshot_bundle_digest='',
				    result_receipt_digest='',result_bundle_digest='',owner_epoch=$3
				WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$3
				  AND tombstoned_at IS NULL`,
				s.instanceID, candidate.childID, epoch,
			)
			if err := changed(tag.RowsAffected(), err); err != nil {
				return err
			}
			for _, table := range []string{
				"child_blob_read", "child_blob", "child_publication",
				"child_disposition_history", "child_disposition",
			} {
				if _, err := tx.Exec(txCtx, `
					DELETE FROM control.`+table+`
					WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$3`,
					s.instanceID, candidate.childID, epoch,
				); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(txCtx, `
				DELETE FROM control.acceptance
				WHERE instance_id=$1 AND id=$2 AND owner_epoch=$3
				  AND state<>'dispatching'`,
				s.instanceID, candidate.acceptanceID, epoch,
			); err != nil {
				return err
			}
			result.Tombstoned++
			remaining--
		}
		if remaining == 0 {
			return nil
		}
		tag, err := tx.Exec(txCtx, `
			DELETE FROM control.child_lineage
			WHERE (instance_id,child_id) IN (
				SELECT instance_id,child_id FROM control.child_lineage
				WHERE instance_id=$1 AND owner_epoch=$2
				  AND tombstoned_at <= clock_timestamp()-$3::interval
				ORDER BY child_id LIMIT $4
			)
			AND instance_id=$1 AND owner_epoch=$2`,
			s.instanceID, epoch, triggerqueue.ChildTombstoneRetention, remaining,
		)
		if err != nil {
			return err
		}
		result.Deleted = int(tag.RowsAffected())
		remaining -= result.Deleted
		if remaining == 0 {
			return nil
		}
		tag, err = tx.Exec(txCtx, `
			DELETE FROM control.child_occurrence AS occurrence
			WHERE occurrence.instance_id=$1 AND occurrence.owner_epoch=$2
			  AND NOT EXISTS (
				SELECT 1 FROM control.child_lineage AS child
				WHERE child.instance_id=occurrence.instance_id
				  AND child.gaggle=occurrence.gaggle
				  AND child.parent_run_id=occurrence.parent_run_id
				  AND child.occurrence=occurrence.occurrence
			  )
			  AND (occurrence.instance_id,occurrence.gaggle,
			       occurrence.parent_run_id,occurrence.occurrence) IN (
				SELECT instance_id,gaggle,parent_run_id,occurrence
				FROM control.child_occurrence
				WHERE instance_id=$1 AND owner_epoch=$2
				ORDER BY gaggle,parent_run_id,occurrence LIMIT $3
			  )`,
			s.instanceID, epoch, remaining,
		)
		if err != nil {
			return err
		}
		result.OccurrencesDeleted = int(tag.RowsAffected())
		remaining -= result.OccurrencesDeleted
		if remaining == 0 {
			return nil
		}
		tag, err = tx.Exec(txCtx, `
			DELETE FROM control.child_authority AS authority
			WHERE authority.instance_id=$1 AND authority.owner_epoch=$2
			  AND authority.expires_at<=clock_timestamp()
			  AND NOT EXISTS (
				SELECT 1 FROM control.child_lineage AS child
				WHERE child.instance_id=authority.instance_id
				  AND child.gaggle=authority.gaggle
				  AND child.parent_run_id=authority.parent_run_id
				  AND child.occurrence=authority.occurrence
				  AND child.tombstoned_at IS NULL
			  )
			  AND (authority.instance_id,authority.gaggle,
			       authority.parent_run_id,authority.occurrence) IN (
				SELECT instance_id,gaggle,parent_run_id,occurrence
				FROM control.child_authority
				WHERE instance_id=$1 AND owner_epoch=$2
				ORDER BY gaggle,parent_run_id,occurrence LIMIT $3
			  )`,
			s.instanceID, epoch, remaining,
		)
		if err != nil {
			return err
		}
		result.AuthoritiesDeleted = int(tag.RowsAffected())
		remaining -= result.AuthoritiesDeleted
		if remaining == 0 {
			return nil
		}
		tag, err = tx.Exec(txCtx, `
			DELETE FROM control.child_parent AS parent
			WHERE parent.instance_id=$1 AND parent.owner_epoch=$2
			  AND parent.settled_at<=clock_timestamp()-$3::interval
			  AND NOT EXISTS (
				SELECT 1 FROM control.child_lineage AS child
				WHERE child.instance_id=parent.instance_id
				  AND child.gaggle=parent.gaggle
				  AND child.parent_run_id=parent.parent_run_id
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM control.child_authority AS authority
				WHERE authority.instance_id=parent.instance_id
				  AND authority.gaggle=parent.gaggle
				  AND authority.parent_run_id=parent.parent_run_id
			  )
			  AND (parent.instance_id,parent.gaggle,parent.parent_run_id) IN (
				SELECT instance_id,gaggle,parent_run_id FROM control.child_parent
				WHERE instance_id=$1 AND owner_epoch=$2
				ORDER BY gaggle,parent_run_id LIMIT $4
			  )`,
			s.instanceID, epoch, triggerqueue.ChildRetention, remaining,
		)
		if err != nil {
			return err
		}
		result.ParentsDeleted = int(tag.RowsAffected())
		return nil
	})
	return result, err
}
