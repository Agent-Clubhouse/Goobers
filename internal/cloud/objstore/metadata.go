package objstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blobstore"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
)

// ErrPinned means a referenced config generation cannot be pruned.
var ErrPinned = errors.New("config generation is pinned")

// Metadata stores object-backed surrender and configuration pointers.
type Metadata struct {
	pool  *pgxpool.Pool
	blobs interface {
		blobstore.Store
		Remove(context.Context, string) error
		StampGCIntent(context.Context, string, string) (string, error)
		Touch(context.Context, string, []byte) error
		RemoveIfMatch(context.Context, string, string) (bool, error)
	}
}

// NewMetadata constructs the PostgreSQL object metadata store.
func NewMetadata(pool *pgxpool.Pool, blobs interface {
	blobstore.Store
	Remove(context.Context, string) error
	StampGCIntent(context.Context, string, string) (string, error)
	Touch(context.Context, string, []byte) error
	RemoveIfMatch(context.Context, string, string) (bool, error)
}) (*Metadata, error) {
	if pool == nil || blobs == nil {
		return nil, errors.New("cloud object metadata requires PostgreSQL and blob storage")
	}
	return &Metadata{pool: pool, blobs: blobs}, nil
}

// PutSurrender creates one immutable surrender row or accepts an exact replay.
func (m *Metadata) PutSurrender(ctx context.Context, instanceID, runID, stage string, attempt int, doc []byte) error {
	tag, err := m.pool.Exec(ctx, `
		INSERT INTO control.surrender (instance_id, run_id, stage, attempt, doc)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		instanceID, runID, stage, attempt, doc,
	)
	if err != nil {
		return fmt.Errorf("write surrender row: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existing []byte
	err = m.pool.QueryRow(ctx, `
		SELECT doc FROM control.surrender
		WHERE instance_id=$1 AND run_id=$2 AND stage=$3 AND attempt=$4`,
		instanceID, runID, stage, attempt,
	).Scan(&existing)
	if err != nil {
		return fmt.Errorf("read surrender replay: %w", err)
	}
	if !bytes.Equal(existing, doc) {
		return fmt.Errorf("%w: surrender identity has different content", ErrConflict)
	}
	return nil
}

// RecordGeneration writes the archive blob before publishing its database pointer.
func (m *Metadata) RecordGeneration(ctx context.Context, instanceID, digest string, archive []byte) error {
	connection, err := m.acquireBlobLock(ctx, digest)
	if err != nil {
		return err
	}
	defer m.releaseBlobLock(connection, digest)
	if err := m.blobs.Touch(ctx, digest, archive); err != nil {
		return fmt.Errorf("publish config generation blob: %w", err)
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin config generation publication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.config_generation (instance_id, digest, blob_digest)
		VALUES ($1, $2, $2)
		ON CONFLICT (instance_id, digest) DO NOTHING`,
		instanceID, digest,
	); err != nil {
		return fmt.Errorf("record config generation: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM control.blob_gc_candidate WHERE digest=$1", digest); err != nil {
		return fmt.Errorf("cancel config generation GC: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit config generation publication: %w", err)
	}
	return nil
}

// ApplyConfig advances the applied generation monotonically inside a caller's
// fenced transaction. It returns false for a stale generation.
func (m *Metadata) ApplyConfig(ctx context.Context, tx pgx.Tx, instanceID, digest string, generation int64) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO control.applied_config (instance_id, digest, generation)
		VALUES ($1, $2, $3)
		ON CONFLICT (instance_id) DO UPDATE
		SET digest=excluded.digest, generation=excluded.generation, applied_at=now()
		WHERE control.applied_config.generation < excluded.generation`,
		instanceID, digest, generation,
	)
	if err != nil {
		return false, fmt.Errorf("apply config generation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PinGeneration prevents pruning while owner references the generation.
func (m *Metadata) PinGeneration(ctx context.Context, instanceID, digest, owner string) error {
	connection, err := m.acquireBlobLock(ctx, digest)
	if err != nil {
		return err
	}
	defer m.releaseBlobLock(connection, digest)
	if err := m.blobs.Touch(ctx, digest, nil); err != nil {
		return fmt.Errorf("touch pinned config generation: %w", err)
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.config_generation_pin (instance_id, digest, owner)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		instanceID, digest, owner,
	); err != nil {
		return fmt.Errorf("pin config generation: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM control.blob_gc_candidate WHERE digest=$1", digest); err != nil {
		return fmt.Errorf("cancel pinned blob GC: %w", err)
	}
	return tx.Commit(ctx)
}

// PruneGeneration removes an unpinned pointer and marks newly unreferenced
// content for a later retention-gated GC sweep.
func (m *Metadata) PruneGeneration(ctx context.Context, instanceID, digest string) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pinned bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM control.config_generation_pin
			WHERE instance_id=$1 AND digest=$2
			UNION ALL
			SELECT 1 FROM control.applied_config
			WHERE instance_id=$1 AND digest=$2
		)`,
		instanceID, digest,
	).Scan(&pinned); err != nil {
		return fmt.Errorf("check config generation pins: %w", err)
	}
	if pinned {
		return ErrPinned
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM control.config_generation
		WHERE instance_id=$1 AND digest=$2`,
		instanceID, digest,
	)
	if err != nil {
		return fmt.Errorf("delete config generation pointer: %w", err)
	}
	var referenced bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM control.config_generation WHERE blob_digest=$1
		)`,
		digest,
	).Scan(&referenced); err != nil {
		return fmt.Errorf("check config generation blob references: %w", err)
	}
	if tag.RowsAffected() == 1 && !referenced {
		if _, err := tx.Exec(ctx, `
			INSERT INTO control.blob_gc_candidate (digest, unreferenced_at)
			VALUES ($1, clock_timestamp())
			ON CONFLICT (digest) DO NOTHING`,
			digest,
		); err != nil {
			return fmt.Errorf("mark blob GC candidate: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// SweepGC deletes candidates that remain globally unreferenced beyond the
// retention window. The caller must hold the gc lease represented by handle.
func (m *Metadata) SweepGC(
	ctx context.Context,
	manager *cloudlease.Manager,
	handle *cloudlease.Handle,
	retention time.Duration,
) (int, error) {
	if manager == nil || handle == nil || retention <= 0 {
		return 0, errors.New("cloud object GC requires a lease and positive retention")
	}
	rows, err := m.pool.Query(ctx, `
		SELECT digest FROM control.blob_gc_candidate
		WHERE unreferenced_at <= (clock_timestamp()-$1::interval)
		ORDER BY unreferenced_at, digest`,
		retention,
	)
	if err != nil {
		return 0, fmt.Errorf("list blob GC candidates: %w", err)
	}
	var candidates []string
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, digest)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	deleted := 0
	for _, digest := range candidates {
		removed, err := m.sweepCandidate(ctx, manager, handle, digest, retention)
		if err != nil {
			return deleted, err
		}
		if removed {
			deleted++
		}
	}
	return deleted, nil
}

func (m *Metadata) sweepCandidate(
	ctx context.Context,
	manager *cloudlease.Manager,
	handle *cloudlease.Handle,
	digest string,
	retention time.Duration,
) (bool, error) {
	connection, err := m.acquireBlobLock(ctx, digest)
	if err != nil {
		return false, err
	}
	defer m.releaseBlobLock(connection, digest)
	eligible := false
	intentID, err := randomIntentID()
	if err != nil {
		return false, err
	}
	var intentETag string
	err = manager.WithFence(ctx, handle, func(txCtx context.Context, tx pgx.Tx) error {
		var referenced bool
		if err := tx.QueryRow(txCtx, `
			SELECT EXISTS (
				SELECT 1 FROM control.config_generation WHERE blob_digest=$1
			)`,
			digest,
		).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			_, err := tx.Exec(txCtx, "DELETE FROM control.blob_gc_candidate WHERE digest=$1", digest)
			return err
		}
		intentETag, err = m.blobs.StampGCIntent(txCtx, digest, intentID)
		if err != nil {
			if errors.Is(err, blobstore.ErrNotFound) {
				intentETag = ""
			} else {
				return err
			}
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.blob_gc_candidate
			SET delete_started_at=clock_timestamp(), intent_id=$3, intent_etag=$4
			WHERE digest=$1
			  AND unreferenced_at <= (clock_timestamp()-$2::interval)`,
			digest, retention, intentID, intentETag,
		)
		eligible = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err != nil || !eligible {
		return false, err
	}
	deleted := true
	if intentETag != "" {
		deleted, err = m.blobs.RemoveIfMatch(ctx, digest, intentETag)
		if err != nil {
			return false, fmt.Errorf("delete unreferenced blob: %w", err)
		}
	}
	if !deleted {
		return false, nil
	}
	if _, err := connection.Exec(ctx, `
		DELETE FROM control.blob_gc_candidate AS candidate
		WHERE candidate.digest=$1
		  AND candidate.intent_id=$2
		  AND NOT EXISTS (
			SELECT 1 FROM control.config_generation WHERE blob_digest=$1
		  )`,
		digest, intentID,
	); err != nil {
		return false, fmt.Errorf("complete blob GC candidate: %w", err)
	}
	return deleted, nil
}

func randomIntentID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create blob GC intent: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (m *Metadata) acquireBlobLock(ctx context.Context, digest string) (*pgxpool.Conn, error) {
	connection, err := m.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire blob GC connection: %w", err)
	}
	if _, err := connection.Exec(ctx, `
		SELECT pg_advisory_lock(hashtextextended('goobers-blob:' || $1, 0))`,
		digest,
	); err != nil {
		connection.Release()
		return nil, fmt.Errorf("lock blob publication: %w", err)
	}
	return connection, nil
}

func (m *Metadata) releaseBlobLock(connection *pgxpool.Conn, digest string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = connection.Exec(ctx, `
		SELECT pg_advisory_unlock(hashtextextended('goobers-blob:' || $1, 0))`,
		digest,
	)
	connection.Release()
}
