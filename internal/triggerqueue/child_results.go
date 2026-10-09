package triggerqueue

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
)

// Results use the same per-carrier limits and database byte ceiling as forks.
// The existing family pruner deletes both only after terminal acknowledgement.
const childResultSchema = `
ALTER TABLE child_lineages ADD COLUMN result_digest TEXT NOT NULL DEFAULT '';
CREATE TABLE child_results (
 child_id TEXT PRIMARY KEY NOT NULL,
 receipt BLOB NOT NULL CHECK(length(receipt) BETWEEN 1 AND 131072),
 bundle_digest TEXT NOT NULL,
 bundle BLOB NOT NULL CHECK(length(bundle) <= 16777216)
);
CREATE TRIGGER child_release_result AFTER UPDATE OF tombstoned_ns ON child_lineages
WHEN OLD.tombstoned_ns IS NULL AND NEW.tombstoned_ns IS NOT NULL
BEGIN DELETE FROM child_results WHERE child_id=OLD.child_id; END;
`

// Result absence before capture is distinct from missing or altered custody.
var (
	ErrChildResultPending     = errors.New("triggerqueue: child result has not been captured")
	ErrChildResultUnavailable = errors.New("triggerqueue: child result custody is missing or invalid")
)

// ChildResult retains a trusted terminal receipt and optional workspace carrier.
// The owner must verify actual termination before storing it. Cancellation does
// not prevent result capture: it still needs an observed, durable outcome.
type ChildResult struct {
	Receipt       []byte
	ReceiptDigest string
	Bundle        []byte
	BundleDigest  string
}

func (r ChildResult) validate() error {
	if len(r.Receipt) == 0 || len(r.Receipt) > MaxChildSnapshotReceiptBytes || len(r.Bundle) > MaxChildSnapshotBytes || r.ReceiptDigest != "sha256:"+childDigest(r.Receipt) {
		return ErrChildResultUnavailable
	}
	if (len(r.Bundle) == 0 && r.BundleDigest != "") || (len(r.Bundle) != 0 && r.BundleDigest != "sha256:"+childDigest(r.Bundle)) {
		return ErrChildResultUnavailable
	}
	return nil
}

// KeepChildResult pins the first result. Retries cannot replace it, and lost
// custody cannot be repaired by observing a changed workspace after the fact.
func (s *Store) KeepChildResult(ctx context.Context, child ChildRecord, result ChildResult) error {
	if !child.Identity.valid() {
		return ErrChildResultUnavailable
	}
	if err := result.validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(child.Identity)...))
	if err != nil {
		return err
	}
	if current.ChildID != child.ChildID || current.AcceptanceID != child.AcceptanceID || current.ProposalDigest != child.ProposalDigest || !current.TombstonedAt.IsZero() {
		return ErrChildResultUnavailable
	}
	retained, err := readChildResult(ctx, tx, current)
	if err == nil {
		if !bytes.Equal(retained.Receipt, result.Receipt) || !bytes.Equal(retained.Bundle, result.Bundle) {
			return ErrConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, ErrChildResultPending) {
		return err
	}
	if !current.AcknowledgedAt.IsZero() || (current.State.Terminal() && current.ResultRef != result.ReceiptDigest) {
		return ErrTransition
	}
	credit, err := resultStorageCredit(ctx, tx, current.ChildID)
	if err != nil {
		return err
	}
	if err := consumeChildStorage(ctx, tx, current.ChildID, credit, len(result.Receipt)+len(result.Bundle)); err != nil {
		return err
	}
	if result.Bundle == nil {
		result.Bundle = []byte{}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO child_results(child_id,receipt,bundle_digest,bundle) VALUES(?,?,?,?)`, current.ChildID, result.Receipt, result.BundleDigest, result.Bundle); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_lineages SET result_digest=? WHERE child_id=?`, result.ReceiptDigest, current.ChildID); err != nil {
		return err
	}
	return tx.Commit()
}

func readChildResult(ctx context.Context, reader childProposalReader, child ChildRecord) (ChildResult, error) {
	var result ChildResult
	if err := reader.QueryRowContext(ctx, `SELECT result_digest FROM child_lineages WHERE child_id=? AND tombstoned_ns IS NULL`, child.ChildID).Scan(&result.ReceiptDigest); err != nil {
		return result, ErrChildResultUnavailable
	}
	if result.ReceiptDigest == "" {
		return result, ErrChildResultPending
	}
	if err := reader.QueryRowContext(ctx, `SELECT receipt,bundle_digest,bundle FROM child_results WHERE child_id=? AND length(receipt) BETWEEN 1 AND ? AND length(bundle)<=?`, child.ChildID, MaxChildSnapshotReceiptBytes, MaxChildSnapshotBytes).Scan(&result.Receipt, &result.BundleDigest, &result.Bundle); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChildResult{}, ErrChildResultUnavailable
		}
		return ChildResult{}, err
	}
	return result, result.validate()
}

// ChildResult reads only this exact qualified lineage; it is not caller auth.
func (s *Store) ChildResult(ctx context.Context, identity ChildIdentity) (ChildResult, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil {
		return ChildResult{}, err
	}
	return readChildResult(ctx, s.db, child)
}
