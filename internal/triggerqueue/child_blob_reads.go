package triggerqueue

import (
	"context"
	"database/sql"
	"errors"

	"github.com/goobers/goobers/internal/blobstore"
)

const childBlobReadSchema = `
CREATE TABLE child_blob_reads (
 child_id TEXT NOT NULL, contract_digest TEXT NOT NULL, digest TEXT NOT NULL,
 PRIMARY KEY(child_id,contract_digest,digest)
);
CREATE TRIGGER child_release_blob_reads AFTER DELETE ON child_blobs
BEGIN DELETE FROM child_blob_reads WHERE child_id=OLD.child_id AND (digest=OLD.digest OR contract_digest=OLD.digest); END;
`

const childBlobUsageSQL = `SELECT
 (SELECT COUNT(*) FROM child_blobs WHERE child_id=?)+(SELECT COUNT(*) FROM child_blob_reads WHERE child_id=?),
 (SELECT COALESCE(SUM(length(data)+1024),0) FROM child_blobs WHERE child_id=?)+(SELECT COUNT(*)*1024 FROM child_blob_reads WHERE child_id=?)`

// KeepChildAttemptBlob atomically stores supplied bytes and grants this exact
// contract access. Knowing a sibling's digest cannot establish read ownership.
func (s *Store) KeepChildAttemptBlob(ctx context.Context, id ChildIdentity, contract, digest string, data []byte) error {
	if !id.valid() || !blobstore.ValidDigest(contract) || len(data) == 0 || len(data) > MaxChildBlobBytes || digest != "sha256:"+childDigest(data) {
		return ErrChildBlobUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	child, err := keepChildBlob(ctx, tx, id, digest, data)
	if err != nil {
		return err
	}
	if err = allowChildBlobRead(ctx, tx, child, contract, digest); err != nil {
		return err
	}
	return tx.Commit()
}

// AllowChildContractRead is a trusted-host operation for already verified
// immutable contract inputs, never exposed as an HTTP request operation.
func (s *Store) AllowChildContractRead(ctx context.Context, id ChildIdentity, contract, digest string) error {
	if !id.valid() || !blobstore.ValidDigest(contract) || !blobstore.ValidDigest(digest) {
		return ErrChildBlobUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	child, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(id)...))
	if err != nil {
		return err
	}
	if err = allowChildBlobRead(ctx, tx, child, contract, digest); err != nil {
		return err
	}
	return tx.Commit()
}

func allowChildBlobRead(ctx context.Context, tx *sql.Tx, child ChildRecord, contract, digest string) error {
	if !child.TombstonedAt.IsZero() {
		return ErrChildBlobUnavailable
	}
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_blobs WHERE child_id=? AND digest IN (?,?)`, child.ChildID, contract, digest).Scan(&present); err != nil {
		return err
	}
	expected := 2
	if contract == digest {
		expected = 1
	}
	if present != expected {
		return ErrChildBlobUnavailable
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_blob_reads WHERE child_id=? AND contract_digest=? AND digest=?`, child.ChildID, contract, digest).Scan(&present); err != nil {
		return err
	}
	if present != 0 {
		return nil
	}
	if !child.AcknowledgedAt.IsZero() {
		return ErrTransition
	}
	var count, used int
	if err := tx.QueryRowContext(ctx, childBlobUsageSQL, child.ChildID, child.ChildID, child.ChildID, child.ChildID).Scan(&count, &used); err != nil {
		return err
	}
	if count >= MaxChildBlobs || used+1024 > MaxChildBlobCustodyBytes {
		return ErrFull
	}
	if err := consumeChildStorage(ctx, tx, child.ChildID, 1024, 1024); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO child_blob_reads(child_id,contract_digest,digest) VALUES(?,?,?)`, child.ChildID, contract, digest)
	return err
}

// ChildAttemptBlobBounded reads only explicitly owned contract data. The limit
// is applied by SQL before blob allocation and missing membership never widens.
func (s *Store) ChildAttemptBlobBounded(ctx context.Context, id ChildIdentity, contract, digest string, limit int64) ([]byte, error) {
	if !id.valid() || !blobstore.ValidDigest(contract) || limit <= 0 {
		return nil, ErrChildBlobUnavailable
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT b.data FROM child_blobs b JOIN child_lineages c ON b.child_id=c.child_id JOIN child_blob_reads r ON r.child_id=b.child_id AND r.digest=b.digest WHERE c.gaggle=? AND c.parent_run=? AND c.occurrence=? AND c.invocation_key=? AND c.tombstoned_ns IS NULL AND r.contract_digest=? AND b.digest=? AND length(b.data)<=?`, append(childArgs(id), contract, digest, min(limit, MaxChildBlobBytes))...).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChildBlobUnavailable
	}
	if err != nil {
		return nil, err
	}
	if digest != "sha256:"+childDigest(data) {
		return nil, ErrChildBlobUnavailable
	}
	return data, nil
}
