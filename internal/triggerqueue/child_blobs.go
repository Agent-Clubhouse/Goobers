package triggerqueue

import (
	"bytes"
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
	if _, err = keepChildBlob(ctx, tx, id, digest, data); err != nil {
		return err
	}
	return tx.Commit()
}

func keepChildBlob(ctx context.Context, tx *sql.Tx, id ChildIdentity, digest string, data []byte) (ChildRecord, error) {
	child, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(id)...))
	if err != nil {
		return ChildRecord{}, err
	}
	if child.ProposalDigest == "" || !child.TombstonedAt.IsZero() {
		return ChildRecord{}, ErrChildBlobUnavailable
	}
	var existing []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM child_blobs WHERE child_id=? AND digest=? AND length(data)<=25165824`, child.ChildID, digest).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ChildRecord{}, err
	}
	if err == nil {
		if !bytes.Equal(existing, data) {
			return ChildRecord{}, ErrChildBlobUnavailable
		}
		return child, nil
	}
	if !child.AcknowledgedAt.IsZero() {
		return ChildRecord{}, ErrTransition
	}
	var count, used int
	if err = tx.QueryRowContext(ctx, childBlobUsageSQL, child.ChildID, child.ChildID, child.ChildID, child.ChildID).Scan(&count, &used); err != nil {
		return ChildRecord{}, err
	}
	additional := len(data) + 1024
	if count >= MaxChildBlobs || used+additional > MaxChildBlobCustodyBytes {
		return ChildRecord{}, ErrFull
	}
	if count == 0 {
		if err = childByteCapacity(ctx, tx, MaxChildBlobCustodyBytes); err != nil {
			return ChildRecord{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE child_lineages SET reserved_bytes=reserved_bytes+? WHERE child_id=?`, MaxChildBlobCustodyBytes, child.ChildID); err != nil {
			return ChildRecord{}, err
		}
	}
	if err = consumeChildStorage(ctx, tx, child.ChildID, additional, additional); err != nil {
		return ChildRecord{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO child_blobs(child_id,digest,data) VALUES(?,?,?)`, child.ChildID, digest, data); err != nil {
		return ChildRecord{}, err
	}
	return child, nil
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
