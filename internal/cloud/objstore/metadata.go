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

const (
	// DefaultMaxPublishLatency is the minimum gap between GC observations.
	DefaultMaxPublishLatency = 30 * time.Second
	// DefaultPITRRetention matches the maximum configured PostgreSQL restore window.
	DefaultPITRRetention = 35 * 24 * time.Hour
	blobMutationTimeout  = 15 * time.Second
)

type blobMutation interface {
	ETag(context.Context) (string, error)
	Renew(context.Context) error
	Touch(context.Context) error
	StampGCIntent(context.Context, string) (string, error)
	RemoveIfMatch(context.Context, string) (bool, error)
	Release(context.Context) error
}

type metadataBlobStore interface {
	blobstore.Store
	AcquireMutation(context.Context, string, []byte) (blobMutation, error)
	TryAcquireMutation(context.Context, string) (blobMutation, bool, error)
}

// Metadata stores object-backed surrender and configuration pointers.
type Metadata struct {
	pool                       *pgxpool.Pool
	experimentalPhysicalDelete bool
	maxPublishLatency          time.Duration
	pitrRetention              time.Duration
	blobs                      metadataBlobStore
}

// MetadataOptions controls object metadata behavior.
type MetadataOptions struct {
	// ExperimentalPhysicalDelete enables storage deletion pending #7843.
	// It is false by default; soft-delete and storage lifecycle are the backstop.
	ExperimentalPhysicalDelete bool
	// MaxPublishLatency separates the two required unreferenced observations.
	// A publisher holds the same storage lease and a shorter operation deadline.
	MaxPublishLatency time.Duration
	// PITRRetention is the minimum unreferenced age before physical deletion.
	PITRRetention time.Duration
}

// NewMetadataWithOptions constructs metadata with explicit experimental options.
func NewMetadataWithOptions(
	pool *pgxpool.Pool,
	blobs metadataBlobStore,
	options MetadataOptions,
) (*Metadata, error) {
	if pool == nil || blobs == nil {
		return nil, errors.New("cloud object metadata requires PostgreSQL and blob storage")
	}
	if options.MaxPublishLatency == 0 {
		options.MaxPublishLatency = DefaultMaxPublishLatency
	}
	if options.MaxPublishLatency <= 0 {
		return nil, errors.New("cloud object metadata publish latency must be positive")
	}
	if options.PITRRetention == 0 {
		options.PITRRetention = DefaultPITRRetention
	}
	if options.PITRRetention < DefaultPITRRetention {
		return nil, fmt.Errorf(
			"cloud object metadata PITR retention must be at least %s",
			DefaultPITRRetention)
	}
	return &Metadata{
		pool: pool, blobs: blobs,
		experimentalPhysicalDelete: options.ExperimentalPhysicalDelete,
		maxPublishLatency:          options.MaxPublishLatency,
		pitrRetention:              options.PITRRetention,
	}, nil
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
	mutation, err := m.blobs.AcquireMutation(ctx, digest, archive)
	if err != nil {
		return err
	}
	defer releaseMutation(mutation)
	opCtx, cancel := context.WithTimeout(ctx, blobMutationTimeout)
	defer cancel()
	tx, err := m.pool.Begin(opCtx)
	if err != nil {
		return fmt.Errorf("begin config generation publication: %w", err)
	}
	defer func() { _ = tx.Rollback(opCtx) }()
	if _, err := tx.Exec(opCtx, `
		INSERT INTO control.config_generation (instance_id, digest, blob_digest)
		VALUES ($1, $2, $2)
		ON CONFLICT (instance_id, digest) DO NOTHING`,
		instanceID, digest,
	); err != nil {
		return fmt.Errorf("record config generation: %w", err)
	}
	if _, err := tx.Exec(opCtx, "DELETE FROM control.blob_gc_candidate WHERE digest=$1", digest); err != nil {
		return fmt.Errorf("cancel config generation GC: %w", err)
	}
	if err := mutation.Renew(opCtx); err != nil {
		return fmt.Errorf("renew config generation storage lease: %w", err)
	}
	if err := tx.Commit(opCtx); err != nil {
		return fmt.Errorf("commit config generation publication: %w", err)
	}
	if err := mutation.Touch(opCtx); err != nil {
		return fmt.Errorf("touch published config generation: %w", err)
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
	mutation, err := m.blobs.AcquireMutation(ctx, digest, nil)
	if err != nil {
		return err
	}
	defer releaseMutation(mutation)
	opCtx, cancel := context.WithTimeout(ctx, blobMutationTimeout)
	defer cancel()
	tx, err := m.pool.Begin(opCtx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(opCtx) }()
	var inserted bool
	err = tx.QueryRow(opCtx, `
		INSERT INTO control.config_generation_pin (instance_id, digest, owner)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING
		RETURNING true`,
		instanceID, digest, owner,
	).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		inserted, err = false, nil
	}
	if err != nil {
		return fmt.Errorf("pin config generation: %w", err)
	}
	if _, err := tx.Exec(opCtx, "DELETE FROM control.blob_gc_candidate WHERE digest=$1", digest); err != nil {
		return fmt.Errorf("cancel pinned blob GC: %w", err)
	}
	if err := mutation.Renew(opCtx); err != nil {
		return fmt.Errorf("renew pinned generation storage lease: %w", err)
	}
	if err := tx.Commit(opCtx); err != nil {
		return err
	}
	if err := mutation.Touch(opCtx); err != nil {
		if inserted {
			compensateCtx, compensateCancel := context.WithTimeout(
				context.WithoutCancel(ctx), blobMutationTimeout)
			defer compensateCancel()
			_, compensateErr := m.pool.Exec(compensateCtx, `
				DELETE FROM control.config_generation_pin
				WHERE instance_id=$1 AND digest=$2 AND owner=$3`,
				instanceID, digest, owner,
			)
			if compensateErr != nil {
				return errors.Join(
					fmt.Errorf("touch pinned config generation: %w", err),
					fmt.Errorf("compensate pin: %w", compensateErr),
				)
			}
		}
		return fmt.Errorf("touch pinned config generation: %w", err)
	}
	return nil
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
		SELECT control.blob_is_referenced($1)`,
		digest,
	).Scan(&referenced); err != nil {
		return fmt.Errorf("check shared blob references: %w", err)
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
	if retention < m.pitrRetention {
		retention = m.pitrRetention
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
	mutation, busy, err := m.blobs.TryAcquireMutation(ctx, digest)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return false, m.forgetMissingCandidate(ctx, manager, handle, digest)
		}
		return false, fmt.Errorf("acquire blob GC mutation lease: %w", err)
	}
	if busy {
		return false, nil
	}
	defer releaseMutation(mutation)
	opCtx, cancel := context.WithTimeout(ctx, blobMutationTimeout)
	defer cancel()
	currentETag, err := mutation.ETag(opCtx)
	if err != nil {
		return false, fmt.Errorf("read blob GC ETag: %w", err)
	}
	var ready bool
	err = manager.WithFence(opCtx, handle, func(txCtx context.Context, tx pgx.Tx) error {
		var referenced bool
		var observedAt *time.Time
		var observedETag *string
		err := tx.QueryRow(txCtx, `
			SELECT candidate.first_observed_at, candidate.first_observed_etag,
			       control.blob_is_referenced(candidate.digest)
			FROM control.blob_gc_candidate AS candidate
			WHERE candidate.digest=$1
			  AND candidate.unreferenced_at <=
			      (clock_timestamp()-$2::interval)
			FOR UPDATE`,
			digest, retention,
		).Scan(&observedAt, &observedETag, &referenced)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if referenced {
			_, err = tx.Exec(txCtx,
				"DELETE FROM control.blob_gc_candidate WHERE digest=$1", digest)
			return err
		}
		if observedAt == nil || observedETag == nil ||
			*observedETag != currentETag {
			_, err = tx.Exec(txCtx, `
				UPDATE control.blob_gc_candidate
				SET first_observed_at=clock_timestamp(),
				    first_observed_etag=$2,
				    delete_started_at=NULL, intent_id=NULL, intent_etag=NULL
				WHERE digest=$1`,
				digest, currentETag,
			)
			return err
		}
		var separated bool
		if err := tx.QueryRow(txCtx, `
			SELECT $1 <= (clock_timestamp()-$2::interval)`,
			*observedAt, m.maxPublishLatency,
		).Scan(&separated); err != nil {
			return err
		}
		ready = separated
		return nil
	})
	if err != nil || !ready {
		return false, err
	}

	intentID, err := randomIntentID()
	if err != nil {
		return false, err
	}
	intentETag, err := mutation.StampGCIntent(opCtx, intentID)
	if err != nil {
		return false, fmt.Errorf("stamp blob GC intent: %w", err)
	}
	eligible := false
	err = manager.WithFence(opCtx, handle, func(txCtx context.Context, tx pgx.Tx) error {
		var referenced bool
		var observedAt *time.Time
		var observedETag *string
		if err := tx.QueryRow(txCtx, `
			SELECT candidate.first_observed_at, candidate.first_observed_etag,
			       control.blob_is_referenced(candidate.digest)
			FROM control.blob_gc_candidate AS candidate
			WHERE candidate.digest=$1
			  AND candidate.unreferenced_at <=
			      (clock_timestamp()-$2::interval)
			FOR UPDATE`,
			digest, retention,
		).Scan(&observedAt, &observedETag, &referenced); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if referenced {
			_, err := tx.Exec(txCtx,
				"DELETE FROM control.blob_gc_candidate WHERE digest=$1", digest)
			return err
		}
		if observedAt == nil || observedETag == nil ||
			*observedETag != currentETag {
			return nil
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.blob_gc_candidate
			SET delete_started_at=clock_timestamp(), intent_id=$3, intent_etag=$4
			WHERE digest=$1
			  AND unreferenced_at <= (clock_timestamp()-$2::interval)
			  AND first_observed_at <=
			      (clock_timestamp()-$5::interval)
			  AND first_observed_etag=$6`,
			digest, retention, intentID, intentETag,
			m.maxPublishLatency, currentETag,
		)
		eligible = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err != nil || !eligible {
		return false, err
	}
	// #7843: storage deletion stays off until global reference/PITR and
	// production-service qualification are complete. Candidate intent remains.
	if !m.experimentalPhysicalDelete {
		return false, nil
	}
	deleted, err := mutation.RemoveIfMatch(opCtx, intentETag)
	if err != nil {
		return false, fmt.Errorf("delete unreferenced blob: %w", err)
	}
	if !deleted {
		return false, nil
	}
	return m.completeCandidate(opCtx, manager, handle, digest, intentID)
}

func (m *Metadata) forgetMissingCandidate(
	ctx context.Context,
	manager *cloudlease.Manager,
	handle *cloudlease.Handle,
	digest string,
) error {
	err := manager.WithFence(ctx, handle, func(txCtx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(txCtx, `
			DELETE FROM control.blob_gc_candidate AS candidate
			WHERE candidate.digest=$1
			  AND NOT control.blob_is_referenced(candidate.digest)`,
			digest,
		)
		return err
	})
	return err
}

func (m *Metadata) completeCandidate(
	ctx context.Context,
	manager *cloudlease.Manager,
	handle *cloudlease.Handle,
	digest string,
	intentID string,
) (bool, error) {
	var completed bool
	err := manager.WithFence(ctx, handle, func(txCtx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(txCtx, `
			DELETE FROM control.blob_gc_candidate AS candidate
			WHERE candidate.digest=$1
			  AND candidate.intent_id=$2
			  AND NOT control.blob_is_referenced(candidate.digest)`,
			digest, intentID,
		)
		completed = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("complete blob GC candidate: %w", err)
	}
	return completed, nil
}

func releaseMutation(mutation blobMutation) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = mutation.Release(ctx)
}

func randomIntentID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create blob GC intent: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
