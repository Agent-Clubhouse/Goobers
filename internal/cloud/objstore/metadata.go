package objstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blobstore"
)

// ErrPinned means a referenced config generation cannot be pruned.
var ErrPinned = errors.New("config generation is pinned")

// Metadata stores object-backed surrender and configuration pointers.
type Metadata struct {
	pool  *pgxpool.Pool
	blobs interface {
		blobstore.Store
		Remove(context.Context, string) error
	}
}

// NewMetadata constructs the PostgreSQL object metadata store.
func NewMetadata(pool *pgxpool.Pool, blobs interface {
	blobstore.Store
	Remove(context.Context, string) error
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
	if err := m.blobs.Put(ctx, digest, archive); err != nil {
		return fmt.Errorf("publish config generation blob: %w", err)
	}
	_, err := m.pool.Exec(ctx, `
		INSERT INTO control.config_generation (instance_id, digest, blob_digest)
		VALUES ($1, $2, $2)
		ON CONFLICT (instance_id, digest) DO NOTHING`,
		instanceID, digest,
	)
	if err != nil {
		return fmt.Errorf("record config generation: %w", err)
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
	_, err := m.pool.Exec(ctx, `
		INSERT INTO control.config_generation_pin (instance_id, digest, owner)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		instanceID, digest, owner,
	)
	if err != nil {
		return fmt.Errorf("pin config generation: %w", err)
	}
	return nil
}

// PruneGeneration removes an unpinned pointer and then its content blob.
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
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if tag.RowsAffected() == 1 && !referenced {
		if err := m.blobs.Remove(ctx, digest); err != nil {
			return fmt.Errorf("delete config generation blob: %w", err)
		}
	}
	return nil
}
