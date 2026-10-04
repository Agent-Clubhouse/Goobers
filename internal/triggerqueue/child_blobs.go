package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
)

// Child execution custody shares the queue's global byte ceiling. It never
// falls through to the unbounded shared blob store. Limits include row overhead.
const (
	MaxChildBlobBytes        = 24 << 20
	MaxChildBlobCustodyBytes = 64 << 20
	MaxChildBlobs            = 512
)

const childBlobSchema = `
CREATE TABLE child_blobs (
 child_id TEXT NOT NULL, digest TEXT NOT NULL,
 data BLOB NOT NULL CHECK(length(data) BETWEEN 1 AND 25165824),
 PRIMARY KEY(child_id,digest)
);
CREATE TRIGGER child_release_blobs AFTER UPDATE OF tombstoned_ns ON child_lineages
WHEN OLD.tombstoned_ns IS NULL AND NEW.tombstoned_ns IS NOT NULL
BEGIN DELETE FROM child_blobs WHERE child_id=OLD.child_id; END;
`

// ErrChildBlobUnavailable does not disclose whether a foreign child owns data.
var ErrChildBlobUnavailable = errors.New("triggerqueue: child blob custody unavailable")

// KeepChildBlob stores immutable bytes only for this exact retained lineage.
// The first blob reserves the maximum execution carrier budget before dispatch.
// Host/pod authentication and current source checks precede this storage API.
func (s *Store) KeepChildBlob(ctx context.Context, id ChildIdentity, digest string, data []byte) error {
	if !id.valid() || len(data) == 0 || len(data) > MaxChildBlobBytes || digest != "sha256:"+childDigest(data) {
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
	if child.ProposalDigest == "" || !child.TombstonedAt.IsZero() {
		return ErrChildBlobUnavailable
	}
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_blobs WHERE child_id=? AND digest=?`, child.ChildID, digest).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return tx.Commit()
	}
	if !child.AcknowledgedAt.IsZero() {
		return ErrTransition
	}
	var count, used int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(data)+1024),0) FROM child_blobs WHERE child_id=?`, child.ChildID).Scan(&count, &used); err != nil {
		return err
	}
	additional := len(data) + 1024
	if count >= MaxChildBlobs || used+additional > MaxChildBlobCustodyBytes {
		return ErrFull
	}
	if count == 0 {
		if err = childByteCapacity(ctx, tx, MaxChildBlobCustodyBytes); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE child_lineages SET reserved_bytes=reserved_bytes+? WHERE child_id=?`, MaxChildBlobCustodyBytes, child.ChildID); err != nil {
			return err
		}
	}
	if err = consumeChildStorage(ctx, tx, child.ChildID, additional, additional); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO child_blobs(child_id,digest,data) VALUES(?,?,?)`, child.ChildID, digest, data); err != nil {
		return err
	}
	return tx.Commit()
}

// ChildBlob verifies data under an exact owner; no global digest lookup exists.
func (s *Store) ChildBlob(ctx context.Context, id ChildIdentity, digest string) ([]byte, error) {
	return s.ChildBlobBounded(ctx, id, digest, MaxChildBlobBytes)
}

// ChildBlobBounded enforces the consumer limit in SQL before allocating data.
func (s *Store) ChildBlobBounded(ctx context.Context, id ChildIdentity, digest string, limit int64) ([]byte, error) {
	if !id.valid() || limit <= 0 {
		return nil, ErrChildBlobUnavailable
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT b.data FROM child_blobs b JOIN child_lineages c ON b.child_id=c.child_id WHERE c.gaggle=? AND c.parent_run=? AND c.occurrence=? AND c.invocation_key=? AND c.tombstoned_ns IS NULL AND b.digest=? AND length(b.data)<=?`, append(childArgs(id), digest, min(limit, MaxChildBlobBytes))...).Scan(&data)
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
