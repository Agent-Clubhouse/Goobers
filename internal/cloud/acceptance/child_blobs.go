package acceptance

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// KeepChildBlob stores immutable child-owned carrier bytes by digest.
func (s *Store) KeepChildBlob(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	digest string,
	data []byte,
) error {
	if !childIdentityValid(identity) || s.childBlobs == nil ||
		len(data) == 0 || len(data) > triggerqueue.MaxChildBlobBytes ||
		!verifyDigestData(digest, data) {
		return triggerqueue.ErrChildBlobUnavailable
	}
	if err := s.childBlobs.Put(ctx, digest, data); err != nil {
		return fmt.Errorf("cloud acceptance: store child blob: %w", err)
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		_, err := s.keepChildBlobTx(txCtx, tx, identity, digest, len(data), epoch)
		return err
	})
}

func (s *Store) keepChildBlobTx(
	ctx context.Context,
	tx pgx.Tx,
	identity triggerqueue.ChildIdentity,
	digest string,
	size int,
	epoch int64,
) (triggerqueue.ChildRecord, error) {
	child, err := s.lockChild(ctx, tx, identity, epoch)
	if err != nil || !child.TombstonedAt.IsZero() {
		return child, triggerqueue.ErrChildBlobUnavailable
	}
	var count int
	var used int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*),COALESCE(sum(size_bytes+1024),0)
		FROM control.child_blob
		WHERE instance_id=$1 AND child_id=$2 AND kind='carrier'`,
		s.instanceID, child.ChildID,
	).Scan(&count, &used); err != nil {
		return child, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM control.child_blob
			WHERE instance_id=$1 AND child_id=$2 AND kind='carrier' AND digest=$3
		)`,
		s.instanceID, child.ChildID, digest,
	).Scan(&exists); err != nil {
		return child, err
	}
	if exists {
		return child, nil
	}
	if !child.AcknowledgedAt.IsZero() {
		return child, triggerqueue.ErrTransition
	}
	if count >= triggerqueue.MaxChildBlobs ||
		used+int64(size+1024) > triggerqueue.MaxChildBlobCustodyBytes {
		return child, triggerqueue.ErrFull
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO control.child_blob (
			instance_id,child_id,digest,kind,size_bytes,owner_epoch
		) VALUES ($1,$2,$3,'carrier',$4,$5)`,
		s.instanceID, child.ChildID, digest, size, epoch,
	)
	return child, err
}

// KeepChildAttemptBlob atomically stores bytes and grants one contract read.
func (s *Store) KeepChildAttemptBlob(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	contract string,
	digest string,
	data []byte,
) error {
	if !childIdentityValid(identity) || s.childBlobs == nil ||
		!blobstore.ValidDigest(contract) ||
		len(data) == 0 || len(data) > triggerqueue.MaxChildBlobBytes ||
		!verifyDigestData(digest, data) {
		return triggerqueue.ErrChildBlobUnavailable
	}
	if err := s.childBlobs.Put(ctx, digest, data); err != nil {
		return err
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.keepChildBlobTx(
			txCtx, tx, identity, digest, len(data), epoch)
		if err != nil {
			return err
		}
		return s.allowChildBlobReadTx(txCtx, tx, child, contract, digest, epoch)
	})
}

// AllowChildContractRead grants an exact already-retained contract/blob pair.
func (s *Store) AllowChildContractRead(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	contract string,
	digest string,
) error {
	if !childIdentityValid(identity) ||
		!blobstore.ValidDigest(contract) || !blobstore.ValidDigest(digest) {
		return triggerqueue.ErrChildBlobUnavailable
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, identity, epoch)
		if err != nil {
			return err
		}
		return s.allowChildBlobReadTx(txCtx, tx, child, contract, digest, epoch)
	})
}

func (s *Store) allowChildBlobReadTx(
	ctx context.Context,
	tx pgx.Tx,
	child triggerqueue.ChildRecord,
	contract string,
	digest string,
	epoch int64,
) error {
	if !child.TombstonedAt.IsZero() {
		return triggerqueue.ErrChildBlobUnavailable
	}
	var present int
	if err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT digest) FROM control.child_blob
		WHERE instance_id=$1 AND child_id=$2 AND digest=ANY($3)`,
		s.instanceID, child.ChildID, []string{contract, digest},
	).Scan(&present); err != nil {
		return err
	}
	expected := 2
	if contract == digest {
		expected = 1
	}
	if present != expected {
		return triggerqueue.ErrChildBlobUnavailable
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO control.child_blob_read (
			instance_id,child_id,contract_digest,digest,owner_epoch
		) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT DO NOTHING`,
		s.instanceID, child.ChildID, contract, digest, epoch,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return nil
}

// ChildAttemptBlobBounded reads only explicitly granted attempt data.
func (s *Store) ChildAttemptBlobBounded(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	contract string,
	digest string,
	limit int64,
) ([]byte, error) {
	if !childIdentityValid(identity) || s.childBlobs == nil ||
		!blobstore.ValidDigest(contract) || limit <= 0 {
		return nil, triggerqueue.ErrChildBlobUnavailable
	}
	var size int64
	err := s.pool.QueryRow(ctx, `
		SELECT b.size_bytes
		FROM control.child_blob AS b
		JOIN control.child_blob_read AS r
		  ON r.instance_id=b.instance_id AND r.child_id=b.child_id AND r.digest=b.digest
		JOIN control.child_lineage AS c
		  ON c.instance_id=b.instance_id AND c.child_id=b.child_id
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.occurrence=$4 AND c.invocation_key=$5
		  AND c.tombstoned_at IS NULL
		  AND r.contract_digest=$6 AND b.digest=$7 AND b.size_bytes<=$8`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
		contract, digest, min(limit, int64(triggerqueue.MaxChildBlobBytes)),
	).Scan(&size)
	if err != nil {
		return nil, triggerqueue.ErrChildBlobUnavailable
	}
	data, err := s.childBlobs.Get(ctx, digest)
	if err != nil || int64(len(data)) != size || !verifyDigestData(digest, data) {
		return nil, triggerqueue.ErrChildBlobUnavailable
	}
	return data, nil
}

// ChildBlobBounded reads a child-scoped blob without a global digest lookup.
func (s *Store) ChildBlobBounded(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	digest string,
	limit int64,
) ([]byte, error) {
	if !childIdentityValid(identity) || s.childBlobs == nil || limit <= 0 {
		return nil, triggerqueue.ErrChildBlobUnavailable
	}
	var size int64
	err := s.pool.QueryRow(ctx, `
		SELECT b.size_bytes
		FROM control.child_blob AS b
		JOIN control.child_lineage AS c
		  ON c.instance_id=b.instance_id AND c.child_id=b.child_id
		WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
		  AND c.occurrence=$4 AND c.invocation_key=$5
		  AND c.tombstoned_at IS NULL
		  AND b.kind='carrier' AND b.digest=$6 AND b.size_bytes<=$7`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
		digest, min(limit, int64(triggerqueue.MaxChildBlobBytes)),
	).Scan(&size)
	if err != nil {
		return nil, triggerqueue.ErrChildBlobUnavailable
	}
	data, err := s.childBlobs.Get(ctx, digest)
	if err != nil || int64(len(data)) != size || !verifyDigestData(digest, data) {
		return nil, triggerqueue.ErrChildBlobUnavailable
	}
	return data, nil
}

// ChildBlob reads one child-scoped blob at the backend maximum.
func (s *Store) ChildBlob(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	digest string,
) ([]byte, error) {
	return s.ChildBlobBounded(ctx, identity, digest, triggerqueue.MaxChildBlobBytes)
}

// KeepChildSnapshot pins immutable parent receipt and workspace carrier bytes.
func (s *Store) KeepChildSnapshot(
	ctx context.Context,
	expected triggerqueue.ChildRecord,
	snapshot triggerqueue.ChildSnapshot,
) error {
	if !childIdentityValid(expected.Identity) || s.childBlobs == nil ||
		len(snapshot.Receipt) == 0 ||
		len(snapshot.Receipt) > triggerqueue.MaxChildSnapshotReceiptBytes ||
		len(snapshot.Bundle) == 0 ||
		len(snapshot.Bundle) > triggerqueue.MaxChildSnapshotBytes ||
		!verifyDigestData(snapshot.ReceiptDigest, snapshot.Receipt) ||
		!verifyDigestData(snapshot.BundleDigest, snapshot.Bundle) {
		return triggerqueue.ErrChildSnapshotUnavailable
	}
	if err := s.childBlobs.Put(ctx, snapshot.ReceiptDigest, snapshot.Receipt); err != nil {
		return err
	}
	if err := s.childBlobs.Put(ctx, snapshot.BundleDigest, snapshot.Bundle); err != nil {
		return err
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		current, err := s.lockChild(txCtx, tx, expected.Identity, epoch)
		if err != nil || current.ChildID != expected.ChildID ||
			current.AcceptanceID != expected.AcceptanceID ||
			!current.TombstonedAt.IsZero() || current.CancellationRequested {
			return triggerqueue.ErrChildSnapshotUnavailable
		}
		var receiptDigest, bundleDigest string
		if err := tx.QueryRow(txCtx, `
			SELECT snapshot_receipt_digest,snapshot_bundle_digest
			FROM control.child_lineage
			WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$3`,
			s.instanceID, current.ChildID, epoch,
		).Scan(&receiptDigest, &bundleDigest); err != nil {
			return err
		}
		if receiptDigest != "" || bundleDigest != "" {
			if receiptDigest == snapshot.ReceiptDigest && bundleDigest == snapshot.BundleDigest {
				return nil
			}
			return triggerqueue.ErrConflict
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_lineage
			SET snapshot_receipt_digest=$3,snapshot_bundle_digest=$4,owner_epoch=$5
			WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$5
			  AND snapshot_receipt_digest='' AND snapshot_bundle_digest=''`,
			s.instanceID, current.ChildID,
			snapshot.ReceiptDigest, snapshot.BundleDigest, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		return s.insertChildObjectPointers(txCtx, tx, current.ChildID, epoch,
			childObject{snapshot.ReceiptDigest, "snapshot-receipt", len(snapshot.Receipt)},
			childObject{snapshot.BundleDigest, "snapshot-bundle", len(snapshot.Bundle)})
	})
}

// ChildSnapshot loads and verifies retained snapshot custody.
func (s *Store) ChildSnapshot(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) (triggerqueue.ChildSnapshot, error) {
	if !childIdentityValid(identity) || s.childBlobs == nil {
		return triggerqueue.ChildSnapshot{}, triggerqueue.ErrChildSnapshotUnavailable
	}
	var receiptDigest, bundleDigest string
	err := s.pool.QueryRow(ctx, `
		SELECT snapshot_receipt_digest,snapshot_bundle_digest
		FROM control.child_lineage
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
		  AND occurrence=$4 AND invocation_key=$5 AND tombstoned_at IS NULL`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
	).Scan(&receiptDigest, &bundleDigest)
	if err != nil {
		return triggerqueue.ChildSnapshot{}, triggerqueue.ErrChildSnapshotUnavailable
	}
	if receiptDigest == "" && bundleDigest == "" {
		return triggerqueue.ChildSnapshot{}, triggerqueue.ErrChildSnapshotPending
	}
	receipt, receiptErr := s.childBlobs.Get(ctx, receiptDigest)
	bundle, bundleErr := s.childBlobs.Get(ctx, bundleDigest)
	if receiptErr != nil || bundleErr != nil ||
		!verifyDigestData(receiptDigest, receipt) || !verifyDigestData(bundleDigest, bundle) {
		return triggerqueue.ChildSnapshot{}, triggerqueue.ErrChildSnapshotUnavailable
	}
	return triggerqueue.ChildSnapshot{
		Receipt: receipt, ReceiptDigest: receiptDigest,
		Bundle: bundle, BundleDigest: bundleDigest,
	}, nil
}

// KeepChildResult pins the first verified terminal receipt and optional bundle.
func (s *Store) KeepChildResult(
	ctx context.Context,
	expected triggerqueue.ChildRecord,
	result triggerqueue.ChildResult,
) error {
	if !childIdentityValid(expected.Identity) || s.childBlobs == nil ||
		len(result.Receipt) == 0 ||
		len(result.Receipt) > triggerqueue.MaxChildSnapshotReceiptBytes ||
		len(result.Bundle) > triggerqueue.MaxChildSnapshotBytes ||
		!verifyDigestData(result.ReceiptDigest, result.Receipt) ||
		(len(result.Bundle) == 0) != (result.BundleDigest == "") ||
		(len(result.Bundle) > 0 && !verifyDigestData(result.BundleDigest, result.Bundle)) {
		return triggerqueue.ErrChildResultUnavailable
	}
	if err := s.childBlobs.Put(ctx, result.ReceiptDigest, result.Receipt); err != nil {
		return err
	}
	if len(result.Bundle) > 0 {
		if err := s.childBlobs.Put(ctx, result.BundleDigest, result.Bundle); err != nil {
			return err
		}
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		current, err := s.lockChild(txCtx, tx, expected.Identity, epoch)
		if err != nil || current.ChildID != expected.ChildID ||
			current.AcceptanceID != expected.AcceptanceID ||
			!current.TombstonedAt.IsZero() {
			return triggerqueue.ErrChildResultUnavailable
		}
		var receiptDigest, bundleDigest string
		if err := tx.QueryRow(txCtx, `
			SELECT result_receipt_digest,result_bundle_digest
			FROM control.child_lineage
			WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$3`,
			s.instanceID, current.ChildID, epoch,
		).Scan(&receiptDigest, &bundleDigest); err != nil {
			return err
		}
		if receiptDigest != "" {
			if receiptDigest == result.ReceiptDigest && bundleDigest == result.BundleDigest {
				return nil
			}
			return triggerqueue.ErrConflict
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_lineage
			SET result_receipt_digest=$3,result_bundle_digest=$4,owner_epoch=$5
			WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$5
			  AND result_receipt_digest=''`,
			s.instanceID, current.ChildID,
			result.ReceiptDigest, result.BundleDigest, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		objects := []childObject{{result.ReceiptDigest, "result-receipt", len(result.Receipt)}}
		if len(result.Bundle) > 0 {
			objects = append(objects,
				childObject{result.BundleDigest, "result-bundle", len(result.Bundle)})
		}
		return s.insertChildObjectPointers(txCtx, tx, current.ChildID, epoch, objects...)
	})
}

// ChildResult loads exact retained terminal-result custody.
func (s *Store) ChildResult(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) (triggerqueue.ChildResult, error) {
	if !childIdentityValid(identity) || s.childBlobs == nil {
		return triggerqueue.ChildResult{}, triggerqueue.ErrChildResultUnavailable
	}
	var receiptDigest, bundleDigest string
	err := s.pool.QueryRow(ctx, `
		SELECT result_receipt_digest,result_bundle_digest
		FROM control.child_lineage
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
		  AND occurrence=$4 AND invocation_key=$5 AND tombstoned_at IS NULL`,
		s.instanceID, identity.Gaggle, identity.ParentRunID,
		identity.StageOccurrence, identity.InvocationKey,
	).Scan(&receiptDigest, &bundleDigest)
	if err != nil {
		return triggerqueue.ChildResult{}, triggerqueue.ErrChildResultUnavailable
	}
	if receiptDigest == "" {
		return triggerqueue.ChildResult{}, triggerqueue.ErrChildResultPending
	}
	receipt, err := s.childBlobs.Get(ctx, receiptDigest)
	if err != nil || !verifyDigestData(receiptDigest, receipt) {
		return triggerqueue.ChildResult{}, triggerqueue.ErrChildResultUnavailable
	}
	var bundle []byte
	if bundleDigest != "" {
		bundle, err = s.childBlobs.Get(ctx, bundleDigest)
		if err != nil || !verifyDigestData(bundleDigest, bundle) {
			return triggerqueue.ChildResult{}, triggerqueue.ErrChildResultUnavailable
		}
	}
	return triggerqueue.ChildResult{
		Receipt: receipt, ReceiptDigest: receiptDigest,
		Bundle: bundle, BundleDigest: bundleDigest,
	}, nil
}

type childObject struct {
	digest string
	kind   string
	size   int
}

func (s *Store) insertChildObjectPointers(
	ctx context.Context,
	tx pgx.Tx,
	childID string,
	epoch int64,
	objects ...childObject,
) error {
	for _, object := range objects {
		if _, err := tx.Exec(ctx, `
			INSERT INTO control.child_blob (
				instance_id,child_id,digest,kind,size_bytes,owner_epoch
			) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT DO NOTHING`,
			s.instanceID, childID, object.digest, object.kind, object.size, epoch,
		); err != nil {
			return err
		}
	}
	return nil
}
