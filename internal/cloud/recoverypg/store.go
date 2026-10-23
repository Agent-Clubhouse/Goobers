// Package recoverypg indexes recovery snapshot records in PostgreSQL. The
// bundle itself lives in object storage under BundleKey. A record's identity
// is immutable and its retention, computed on the database clock, only grows.
package recoverypg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
)

var (
	// ErrConflict means the run already has a record with another identity.
	ErrConflict = errors.New("recoverypg: run has a different recovery record")
	// ErrNotFound means no record exists for the run.
	ErrNotFound = errors.New("recoverypg: recovery record not found")
)

// Record is the immutable identity of one published snapshot. IdentityTime
// is stored, compared and returned in UTC at microsecond precision.
type Record struct {
	RunID         string
	RepositoryKey string
	Ref           string
	BaseRef       string
	BaseSHA       string
	SnapshotSHA   string
	PatchDigest   string
	ArchiveDigest string
	ArchiveBytes  int64
	ArchiveFormat string
	BundleKey     string
	IdentityTime  time.Time
}

// Stored is a record with its database-clock bookkeeping.
type Stored struct {
	Record
	CreatedAt   time.Time
	RetainUntil time.Time
	OwnerEpoch  int64
}

// Store is the PostgreSQL recovery-record index for one lease.
type Store struct {
	Pool  *pgxpool.Pool
	Fence *lease.Manager
}

const columns = `run_id, repository_key, ref, base_ref, base_sha, snapshot_sha, patch_digest,
	archive_digest, archive_bytes, archive_format, bundle_key, identity_time,
	created_at, retain_until, owner_epoch`

func scan(row pgx.Row) (Stored, error) {
	var s Stored
	err := row.Scan(&s.RunID, &s.RepositoryKey, &s.Ref, &s.BaseRef, &s.BaseSHA, &s.SnapshotSHA,
		&s.PatchDigest, &s.ArchiveDigest, &s.ArchiveBytes, &s.ArchiveFormat, &s.BundleKey,
		&s.IdentityTime, &s.CreatedAt, &s.RetainUntil, &s.OwnerEpoch)
	s.IdentityTime, s.CreatedAt, s.RetainUntil = s.IdentityTime.UTC(), s.CreatedAt.UTC(), s.RetainUntil.UTC()
	return s, err
}

func (r Record) normalized() Record {
	if r.ArchiveFormat == "" {
		r.ArchiveFormat = "full"
	}
	r.IdentityTime = r.IdentityTime.UTC().Truncate(time.Microsecond)
	return r
}

// Publish records r with retention measured from the database clock. An
// identical retry resolves to the stored record and extends its retention;
// a different identity for the same run is ErrConflict.
func (s Store) Publish(ctx context.Context, handle *lease.Handle, r Record, retention time.Duration) (Stored, error) {
	r = r.normalized()
	if r.RunID == "" || r.BundleKey == "" || r.IdentityTime.IsZero() || retention <= 0 {
		return Stored{}, errors.New("recoverypg: invalid record")
	}
	var stored Stored
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		found, err := resolve(ctx, tx, handle, r, retention, &stored)
		if found || err != nil {
			return err
		}
		stored, err = scan(tx.QueryRow(ctx, `
			INSERT INTO control.recovery_record (instance_id, run_id, repository_key, ref, base_ref,
				base_sha, snapshot_sha, patch_digest, archive_digest, archive_bytes, archive_format,
				bundle_key, identity_time, retain_until, owner_epoch)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now()+$14::interval, $15)
			ON CONFLICT DO NOTHING
			RETURNING `+columns,
			handle.InstanceID, r.RunID, r.RepositoryKey, r.Ref, r.BaseRef, r.BaseSHA, r.SnapshotSHA,
			r.PatchDigest, r.ArchiveDigest, r.ArchiveBytes, r.ArchiveFormat, r.BundleKey,
			r.IdentityTime, retention, handle.Epoch))
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent publish committed the run first; resolve against it.
			if found, err = resolve(ctx, tx, handle, r, retention, &stored); !found && err == nil {
				err = ErrNotFound
			}
			return err
		}
		if err != nil {
			return fmt.Errorf("insert recovery record: %w", err)
		}
		return nil
	})
	return stored, err
}

// resolve reports whether runID has a record; an identical one is renewed.
func resolve(ctx context.Context, tx pgx.Tx, handle *lease.Handle, r Record, retention time.Duration, stored *Stored) (bool, error) {
	existing, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM control.recovery_record
		WHERE instance_id=$1 AND run_id=$2 FOR UPDATE`, handle.InstanceID, r.RunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read recovery record: %w", err)
	}
	if existing.normalized() != r {
		return true, ErrConflict
	}
	*stored, err = renew(ctx, tx, handle, r.RunID, retention)
	return true, err
}

// Renew extends retention to at least now()+retention on the database clock.
// Retention never shrinks: a shorter request leaves the deadline unchanged.
func (s Store) Renew(ctx context.Context, handle *lease.Handle, runID string, retention time.Duration) (Stored, error) {
	if retention <= 0 {
		return Stored{}, errors.New("recoverypg: retention must be positive")
	}
	var stored Stored
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = renew(ctx, tx, handle, runID, retention)
		return err
	})
	return stored, err
}

func renew(ctx context.Context, tx pgx.Tx, handle *lease.Handle, runID string, retention time.Duration) (Stored, error) {
	stored, err := scan(tx.QueryRow(ctx, `
		UPDATE control.recovery_record
		SET retain_until=GREATEST(retain_until, now()+$3::interval), owner_epoch=$4
		WHERE instance_id=$1 AND run_id=$2 AND owner_epoch <= $4
		RETURNING `+columns,
		handle.InstanceID, runID, retention, handle.Epoch))
	if errors.Is(err, pgx.ErrNoRows) {
		return Stored{}, ErrNotFound
	}
	if err != nil {
		return Stored{}, fmt.Errorf("renew recovery record: %w", err)
	}
	return stored, nil
}

// Get reads the effective record for runID.
func (s Store) Get(ctx context.Context, instanceID, runID string) (Stored, error) {
	stored, err := scan(s.Pool.QueryRow(ctx, `SELECT `+columns+` FROM control.recovery_record
		WHERE instance_id=$1 AND run_id=$2`, instanceID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Stored{}, ErrNotFound
	}
	if err != nil {
		return Stored{}, fmt.Errorf("read recovery record: %w", err)
	}
	return stored, nil
}

// Reap deletes up to limit records whose retention has passed on the database
// clock and returns them. The caller deletes each bundle afterwards; a bundle
// left behind by a failed delete is unreferenced garbage, never a dangling row.
func (s Store) Reap(ctx context.Context, handle *lease.Handle, limit int) ([]Stored, error) {
	if limit <= 0 {
		return nil, nil
	}
	var reaped []Stored
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			DELETE FROM control.recovery_record AS r
			USING (
				SELECT run_id FROM control.recovery_record
				WHERE instance_id=$1 AND retain_until < now() AND owner_epoch <= $2
				ORDER BY retain_until, run_id
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			) AS due
			WHERE r.instance_id=$1 AND r.run_id=due.run_id
			RETURNING `+qualified,
			handle.InstanceID, handle.Epoch, limit)
		if err != nil {
			return fmt.Errorf("reap recovery records: %w", err)
		}
		reaped, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stored, error) { return scan(row) })
		return err
	})
	if err != nil {
		return nil, err
	}
	return reaped, nil
}

const qualified = `r.run_id, r.repository_key, r.ref, r.base_ref, r.base_sha, r.snapshot_sha,
	r.patch_digest, r.archive_digest, r.archive_bytes, r.archive_format, r.bundle_key,
	r.identity_time, r.created_at, r.retain_until, r.owner_epoch`
