// Package recoverypg indexes retained recovery snapshots in PostgreSQL. Each
// bundle lives in object storage under its bundle key. A record's identity is
// immutable and its retention, computed on the database clock, only grows.
package recoverypg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/recovery"
)

// Store is the PostgreSQL recovery-record index for one lease. Bind adapts it
// to recovery.RecordIndex.
type Store struct {
	Pool  *pgxpool.Pool
	Fence *lease.Manager
}

const columns = `version, repository_key, run_id, snapshot_sha, ref, base_ref, base_sha,
	patch_digest, archive_digest, archive_bytes, archive_format, identity_unix_ns,
	retain_until, bundle_key`

const qualified = `r.version, r.repository_key, r.run_id, r.snapshot_sha, r.ref, r.base_ref,
	r.base_sha, r.patch_digest, r.archive_digest, r.archive_bytes, r.archive_format,
	r.identity_unix_ns, r.retain_until, r.bundle_key`

func scan(row pgx.Row) (recovery.IndexedRecord, error) {
	var indexed recovery.IndexedRecord
	r := &indexed.Record
	var identityNS int64
	err := row.Scan(&r.Version, &r.RepositoryKey, &r.RunID, &r.SnapshotSHA, &r.Ref, &r.BaseRef,
		&r.BaseSHA, &r.PatchDigest, &r.ArchiveDigest, &r.ArchiveBytes, &r.ArchiveFormat,
		&identityNS, &r.RetainUntil, &indexed.Bundle)
	r.CreatedAt, r.RetainUntil = time.Unix(0, identityNS).UTC(), r.RetainUntil.UTC()
	return indexed, err
}

func keyArgs(handle *lease.Handle, key recovery.RecordKey) []any {
	return []any{handle.InstanceID, key.RepositoryKey, key.RunID, key.SnapshotSHA}
}

// Publish indexes record, whose bundle is already durable at bundle, with
// RetainUntil set retention past the database clock. record.RetainUntil must
// be zero. An identical retry returns the stored record unchanged; a
// different record for the same key is recovery.ErrRecordConflict.
func (s Store) Publish(ctx context.Context, handle *lease.Handle, record recovery.Record, bundle string, retention time.Duration) (recovery.IndexedRecord, error) {
	if err := validate(record, bundle, retention); err != nil {
		return recovery.IndexedRecord{}, err
	}
	record.CreatedAt = record.CreatedAt.UTC()
	var indexed recovery.IndexedRecord
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		found, err := resolve(ctx, tx, handle, record, &indexed)
		if found || err != nil {
			return err
		}
		if err := bundleAvailable(ctx, tx, handle, bundle); err != nil {
			return err
		}
		indexed, err = scan(tx.QueryRow(ctx, `
			INSERT INTO control.recovery_record (instance_id, repository_key, run_id, snapshot_sha,
				version, ref, base_ref, base_sha, patch_digest, archive_digest, archive_bytes,
				archive_format, bundle_key, identity_unix_ns, retain_until, owner_epoch)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now()+$15::interval, $16)
			ON CONFLICT DO NOTHING
			RETURNING `+columns,
			handle.InstanceID, record.RepositoryKey, record.RunID, record.SnapshotSHA,
			record.Version, record.Ref, record.BaseRef, record.BaseSHA, record.PatchDigest,
			record.ArchiveDigest, record.ArchiveBytes, record.ArchiveFormat, bundle,
			record.CreatedAt.UnixNano(), retention, handle.Epoch))
		if errors.Is(err, pgx.ErrNoRows) {
			// Every unique index is an arbiter (no conflict target), so a racing
			// publish never surfaces as a unique violation. A committed record
			// for the key resolves as an identical retry or ErrRecordConflict;
			// otherwise the conflict was the bundle location, which another
			// record holds.
			if found, err = resolve(ctx, tx, handle, record, &indexed); !found && err == nil {
				err = fmt.Errorf("%w: %s", recovery.ErrBundleUnavailable, bundle)
			}
			return err
		}
		if err != nil {
			return fmt.Errorf("insert recovery record: %w", err)
		}
		// Re-check after the INSERT. A concurrent Reap of the record that held
		// this location may have deleted it and tombstoned the location without
		// committing; the INSERT waits on the unique index until that Reap
		// commits, and only this fresh read sees its tombstone.
		return bundleAvailable(ctx, tx, handle, bundle)
	})
	return indexed, err
}

// bundleAvailable refuses a location that a reap handed back for deletion
// and that has not been released. Another record holding the location is
// refused by the unique constraint.
func bundleAvailable(ctx context.Context, tx pgx.Tx, handle *lease.Handle, bundle string) error {
	var retired bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM control.recovery_bundle_tombstone WHERE instance_id=$1 AND bundle_key=$2)`,
		handle.InstanceID, bundle).Scan(&retired); err != nil {
		return fmt.Errorf("read recovery bundle tombstone: %w", err)
	}
	if retired {
		return fmt.Errorf("%w: %s", recovery.ErrBundleUnavailable, bundle)
	}
	return nil
}

func validate(record recovery.Record, bundle string, retention time.Duration) error {
	if !record.RetainUntil.IsZero() {
		return errors.New("recoverypg: RetainUntil is set by the index")
	}
	if bundle == "" || retention <= 0 {
		return errors.New("recoverypg: bundle and positive retention are required")
	}
	// The deadline is unknown until the database computes it; validate the
	// record's shape against the earliest deadline it can have.
	record.RetainUntil = record.CreatedAt.Add(retention)
	return record.Validate()
}

// resolve reports whether the key is indexed; an identical record resolves to
// the stored one unchanged.
func resolve(ctx context.Context, tx pgx.Tx, handle *lease.Handle, record recovery.Record, indexed *recovery.IndexedRecord) (bool, error) {
	existing, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM control.recovery_record
		WHERE instance_id=$1 AND repository_key=$2 AND run_id=$3 AND snapshot_sha=$4
		FOR UPDATE`, keyArgs(handle, record.Key())...))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read recovery record: %w", err)
	}
	if !recovery.SameIdentity(existing.Record, record) {
		return true, recovery.ErrRecordConflict
	}
	*indexed = existing
	return true, nil
}

// Renew extends retention to at least now()+retention on the database clock.
// Retention never shrinks: a shorter request leaves the deadline unchanged.
func (s Store) Renew(ctx context.Context, handle *lease.Handle, key recovery.RecordKey, retention time.Duration) (recovery.IndexedRecord, error) {
	if retention <= 0 {
		return recovery.IndexedRecord{}, errors.New("recoverypg: retention must be positive")
	}
	var indexed recovery.IndexedRecord
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		indexed, err = scan(tx.QueryRow(ctx, `
			UPDATE control.recovery_record
			SET retain_until=GREATEST(retain_until, now()+$5::interval), owner_epoch=$6
			WHERE instance_id=$1 AND repository_key=$2 AND run_id=$3 AND snapshot_sha=$4
			  AND owner_epoch <= $6
			RETURNING `+columns,
			append(keyArgs(handle, key), retention, handle.Epoch)...))
		if errors.Is(err, pgx.ErrNoRows) {
			return recovery.ErrRecordNotFound
		}
		if err != nil {
			return fmt.Errorf("renew recovery record: %w", err)
		}
		return nil
	})
	return indexed, err
}

// Get reads the effective record for key.
func (s Store) Get(ctx context.Context, instanceID string, key recovery.RecordKey) (recovery.IndexedRecord, error) {
	indexed, err := scan(s.Pool.QueryRow(ctx, `SELECT `+columns+` FROM control.recovery_record
		WHERE instance_id=$1 AND repository_key=$2 AND run_id=$3 AND snapshot_sha=$4`,
		instanceID, key.RepositoryKey, key.RunID, key.SnapshotSHA))
	if errors.Is(err, pgx.ErrNoRows) {
		return recovery.IndexedRecord{}, recovery.ErrRecordNotFound
	}
	if err != nil {
		return recovery.IndexedRecord{}, fmt.Errorf("read recovery record: %w", err)
	}
	return indexed, nil
}

// Reap deletes up to limit records whose retention has passed on the database
// clock, tombstones their bundles in the same transaction, and returns them.
// The returned list is a convenience: a commit whose outcome is lost still
// tombstoned its bundles, so the reaper deletes from RetiredBundles, never
// from this result alone (#8163). A bundle left behind by a failed delete is
// unreferenced garbage, never a dangling row.
func (s Store) Reap(ctx context.Context, handle *lease.Handle, limit int) ([]recovery.IndexedRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	var reaped []recovery.IndexedRecord
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			-- MATERIALIZED evaluates the LIMIT once: a rescanned subquery would
			-- skip rows this statement already deleted and select new ones.
			WITH due AS MATERIALIZED (
				SELECT repository_key, run_id, snapshot_sha FROM control.recovery_record
				WHERE instance_id=$1 AND retain_until < now() AND owner_epoch <= $2
				ORDER BY retain_until, run_id
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			)
			DELETE FROM control.recovery_record AS r
			USING due
			WHERE r.instance_id=$1 AND r.repository_key=due.repository_key
			  AND r.run_id=due.run_id AND r.snapshot_sha=due.snapshot_sha
			RETURNING `+qualified,
			handle.InstanceID, handle.Epoch, limit)
		if err != nil {
			return fmt.Errorf("reap recovery records: %w", err)
		}
		reaped, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (recovery.IndexedRecord, error) { return scan(row) })
		if err != nil {
			return err
		}
		// Retire each returned location in the same transaction, so no
		// publication can adopt it before the caller deletes the bundle.
		bundles := make([]string, len(reaped))
		for i, indexed := range reaped {
			bundles[i] = indexed.Bundle
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO control.recovery_bundle_tombstone (instance_id, bundle_key)
			SELECT $1, unnest($2::text[]) ON CONFLICT DO NOTHING`,
			handle.InstanceID, bundles); err != nil {
			return fmt.Errorf("retire reaped recovery bundles: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reaped, nil
}

// RetiredBundles lists up to limit tombstoned bundle locations: reaped and not
// yet released, so still to be deleted from object storage. Tombstones are
// written only under the fence, so the read is unfenced, like Get.
func (s Store) RetiredBundles(ctx context.Context, instanceID string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT bundle_key FROM control.recovery_bundle_tombstone
		WHERE instance_id=$1 ORDER BY bundle_key LIMIT $2`, instanceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list retired recovery bundles: %w", err)
	}
	bundles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list retired recovery bundles: %w", err)
	}
	return bundles, nil
}

// ReleaseBundle makes a reaped bundle's location publishable again. Call it
// only after the bundle is deleted from object storage. Releasing a bundle
// that is not retired is a no-op, so a retried release is safe.
func (s Store) ReleaseBundle(ctx context.Context, handle *lease.Handle, bundle string) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			DELETE FROM control.recovery_bundle_tombstone WHERE instance_id=$1 AND bundle_key=$2`,
			handle.InstanceID, bundle); err != nil {
			return fmt.Errorf("release recovery bundle: %w", err)
		}
		return nil
	})
}

// Bind returns the store as recovery.RecordIndex for one acquired lease.
func (s Store) Bind(handle *lease.Handle) recovery.RecordIndex {
	return bound{store: s, handle: handle}
}

type bound struct {
	store  Store
	handle *lease.Handle
}

func (b bound) Publish(ctx context.Context, record recovery.Record, bundle string, retention time.Duration) (recovery.IndexedRecord, error) {
	return b.store.Publish(ctx, b.handle, record, bundle, retention)
}

func (b bound) Renew(ctx context.Context, key recovery.RecordKey, retention time.Duration) (recovery.IndexedRecord, error) {
	return b.store.Renew(ctx, b.handle, key, retention)
}

func (b bound) Get(ctx context.Context, key recovery.RecordKey) (recovery.IndexedRecord, error) {
	return b.store.Get(ctx, b.handle.InstanceID, key)
}

func (b bound) Reap(ctx context.Context, limit int) ([]recovery.IndexedRecord, error) {
	return b.store.Reap(ctx, b.handle, limit)
}

func (b bound) RetiredBundles(ctx context.Context, limit int) ([]string, error) {
	return b.store.RetiredBundles(ctx, b.handle.InstanceID, limit)
}

func (b bound) ReleaseBundle(ctx context.Context, bundle string) error {
	return b.store.ReleaseBundle(ctx, b.handle, bundle)
}
