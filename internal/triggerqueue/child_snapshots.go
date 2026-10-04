package triggerqueue

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
)

// Child snapshot custody shares the trigger database's byte ceiling and reserve.
// Large working changes backpressure intake rather than growing orphan files.
const (
	MaxChildSnapshotBytes        = 16 << 20
	MaxChildSnapshotReceiptBytes = 128 << 10
)

// Missing pre-capture custody is distinct from lost or altered retained bytes.
var (
	ErrChildSnapshotPending     = errors.New("triggerqueue: child workspace has not been captured")
	ErrChildSnapshotUnavailable = errors.New("triggerqueue: child workspace snapshot custody is missing or invalid")
)

// ChildSnapshot contains the exact trusted receipt and bounded Git carrier.
// The owner verifies receipt semantics and repository prerequisites before use.
// A content address alone is never permission to read another child's snapshot.
type ChildSnapshot struct {
	Receipt       []byte
	ReceiptDigest string
	Bundle        []byte
	BundleDigest  string
}

const childSnapshotSchema = `
ALTER TABLE child_lineages ADD COLUMN snapshot_digest TEXT NOT NULL DEFAULT '';
CREATE TABLE child_snapshots (
 child_id TEXT PRIMARY KEY NOT NULL,
 receipt BLOB NOT NULL CHECK(length(receipt) BETWEEN 1 AND 131072),
 bundle_digest TEXT NOT NULL,
 bundle BLOB NOT NULL CHECK(length(bundle) BETWEEN 1 AND 16777216)
);
CREATE TRIGGER child_release_snapshot AFTER UPDATE OF tombstoned_ns ON child_lineages
WHEN OLD.tombstoned_ns IS NULL AND NEW.tombstoned_ns IS NOT NULL
BEGIN DELETE FROM child_snapshots WHERE child_id=OLD.child_id; END;
`

func (snapshot ChildSnapshot) validate() error {
	if len(snapshot.Receipt) < 1 || len(snapshot.Receipt) > MaxChildSnapshotReceiptBytes || len(snapshot.Bundle) < 1 || len(snapshot.Bundle) > MaxChildSnapshotBytes {
		return ErrChildSnapshotUnavailable
	}
	if snapshot.ReceiptDigest != "sha256:"+childDigest(snapshot.Receipt) || snapshot.BundleDigest != "sha256:"+childDigest(snapshot.Bundle) {
		return ErrChildSnapshotUnavailable
	}
	return nil
}

// KeepChildSnapshot atomically pins the first capture to accepted source custody.
// The caller holds exclusive, acknowledged parent workspace custody. A retry
// must present exactly the prior receipt and carrier; it cannot recapture a
// changed parent. Cancellation fences new capture in the same transaction.
func (s *Store) KeepChildSnapshot(ctx context.Context, child ChildRecord, snapshot ChildSnapshot) error {
	if !child.Identity.valid() {
		return ErrChildSnapshotUnavailable
	}
	if err := snapshot.validate(); err != nil {
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
		return ErrChildSnapshotUnavailable
	}
	retained, err := readChildSnapshot(ctx, tx, current)
	if err == nil {
		if !bytes.Equal(retained.Receipt, snapshot.Receipt) || !bytes.Equal(retained.Bundle, snapshot.Bundle) {
			return ErrConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, ErrChildSnapshotPending) {
		return err
	}
	if current.CancellationRequested {
		return ErrParentCancelled
	}
	if current.State != ChildQueued {
		return ErrTransition
	}
	var terminalReceipt string
	if err := tx.QueryRowContext(ctx, `SELECT result_digest FROM child_lineages WHERE child_id=?`, current.ChildID).Scan(&terminalReceipt); err != nil {
		return err
	}
	if terminalReceipt != "" {
		return ErrTransition
	}
	if err := consumeChildStorage(ctx, tx, current.ChildID, childSnapshotAllowance, len(snapshot.Receipt)+len(snapshot.Bundle)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO child_snapshots(child_id,receipt,bundle_digest,bundle) VALUES(?,?,?,?)`, current.ChildID, snapshot.Receipt, snapshot.BundleDigest, snapshot.Bundle); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_lineages SET snapshot_digest=? WHERE child_id=?`, snapshot.ReceiptDigest, current.ChildID); err != nil {
		return err
	}
	return tx.Commit()
}

func readChildSnapshot(ctx context.Context, reader childProposalReader, child ChildRecord) (ChildSnapshot, error) {
	var snapshot ChildSnapshot
	if err := reader.QueryRowContext(ctx, `SELECT snapshot_digest FROM child_lineages WHERE child_id=? AND tombstoned_ns IS NULL`, child.ChildID).Scan(&snapshot.ReceiptDigest); err != nil {
		return snapshot, ErrChildSnapshotUnavailable
	}
	if snapshot.ReceiptDigest == "" {
		return snapshot, ErrChildSnapshotPending
	}
	if err := reader.QueryRowContext(ctx, `SELECT receipt,bundle_digest,bundle FROM child_snapshots WHERE child_id=? AND length(receipt) BETWEEN 1 AND ? AND length(bundle) BETWEEN 1 AND ?`, child.ChildID, MaxChildSnapshotReceiptBytes, MaxChildSnapshotBytes).Scan(&snapshot.Receipt, &snapshot.BundleDigest, &snapshot.Bundle); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChildSnapshot{}, ErrChildSnapshotUnavailable
		}
		return ChildSnapshot{}, err
	}
	return snapshot, snapshot.validate()
}

// ChildSnapshot verifies retained custody for this exact qualified lineage.
// It does not authorize an HTTP caller and never repairs missing storage.
func (s *Store) ChildSnapshot(ctx context.Context, identity ChildIdentity) (ChildSnapshot, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil {
		return ChildSnapshot{}, err
	}
	return readChildSnapshot(ctx, s.db, child)
}
