package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
)

func readChildResult(ctx context.Context, reader childProposalReader, c ChildRecord) (ChildResult, error) {
	if c.ExecutionEpoch == 0 {
		return readInitialChildResult(ctx, reader, c)
	}
	return readChildEpochResult(ctx, reader, c, c.ExecutionEpoch)
}
func readChildEpochResult(ctx context.Context, reader childProposalReader, c ChildRecord, epoch int) (ChildResult, error) {
	var selected string
	if err := reader.QueryRowContext(ctx, `SELECT result_digest FROM child_execution_epochs WHERE child_id=? AND epoch=?`, c.ChildID, epoch).Scan(&selected); err != nil {
		return ChildResult{}, ErrChildResultUnavailable
	}
	if selected == "" {
		return ChildResult{}, ErrChildResultPending
	}
	var r ChildResult
	err := reader.QueryRowContext(ctx, `SELECT receipt,receipt_digest,bundle_digest,bundle FROM child_execution_results WHERE child_id=? AND epoch=? AND length(receipt) BETWEEN 1 AND ? AND length(bundle)<=?`, c.ChildID, epoch, MaxChildSnapshotReceiptBytes, MaxChildSnapshotBytes).Scan(&r.Receipt, &r.ReceiptDigest, &r.BundleDigest, &r.Bundle)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.ReceiptDigest != selected) {
		return ChildResult{}, ErrChildResultUnavailable
	}
	if err != nil {
		return ChildResult{}, err
	}
	return r, r.validate()
}
func keepChildEpochResult(ctx context.Context, tx *sql.Tx, c ChildRecord, r ChildResult) error {
	var prior bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM child_lineages WHERE child_id=? AND result_digest=?) OR EXISTS(SELECT 1 FROM child_execution_epochs WHERE child_id=? AND epoch!=? AND result_digest=?)`, c.ChildID, r.ReceiptDigest, c.ChildID, c.ExecutionEpoch, r.ReceiptDigest).Scan(&prior); err != nil {
		return err
	}
	if prior {
		return ErrConflict
	}
	if err := consumeChildStorage(ctx, tx, c.ChildID, childResultAllowance, len(r.Receipt)+len(r.Bundle)); err != nil {
		return err
	}
	if r.Bundle == nil {
		r.Bundle = []byte{}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO child_execution_results(child_id,epoch,receipt,receipt_digest,bundle_digest,bundle) VALUES(?,?,?,?,?,?)`, c.ChildID, c.ExecutionEpoch, r.Receipt, r.ReceiptDigest, r.BundleDigest, r.Bundle); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE child_execution_epochs SET result_digest=? WHERE child_id=? AND epoch=?`, r.ReceiptDigest, c.ChildID, c.ExecutionEpoch)
	return err
}

// ChildExecutionResult reads selected immutable custody, including the original
// result after restart. It does not make that prior result eligible for a parent
// disposition or grant execution authority to its run identity.
func (s *Store) ChildExecutionResult(ctx context.Context, id ChildIdentity, runID string) (ChildResult, error) {
	c, err := s.GetChild(ctx, id)
	if err != nil {
		return ChildResult{}, err
	}
	if !c.TombstonedAt.IsZero() {
		return ChildResult{}, ErrChildResultUnavailable
	}
	if runID == c.RunID {
		return readInitialChildResult(ctx, s.db, c)
	}
	e, err := readChildExecution(ctx, s.db, c, runID)
	if err != nil {
		return ChildResult{}, err
	}
	return readChildEpochResult(ctx, s.db, c, e.Epoch)
}
