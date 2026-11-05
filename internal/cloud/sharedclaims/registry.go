// Package sharedclaims binds cloud claims to immutable workflow pins and
// retains repository custody for non-authoritative label reconciliation.
package sharedclaims

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

const maxRepositories = 4096

// Registry retains repositories independently of claims and run retention.
// All mutations use the controller pool and its live lease fence.
type Registry struct {
	Pool       *pgxpool.Pool
	InstanceID string
	Fence      *lease.Manager
	Owner      func() *lease.Handle
}

// Register takes durable retry custody before any remote transition. Repeated
// registration preserves the sweep cursor and never stores credentials.
func (r Registry) Register(ctx context.Context, repo providers.RepositoryRef) error {
	if r.Owner == nil {
		return errors.New("shared registry requires a controller owner")
	}
	if repo.Provider != providers.ProviderGitHub || repo.Owner == "" || repo.Name == "" {
		return errors.New("shared repository must be a complete GitHub identity")
	}
	raw, err := json.Marshal(repo)
	if err != nil {
		return err
	}
	if len(raw) > 4096 {
		return errors.New("shared repository exceeds its bound")
	}
	return r.fenced(ctx, r.Owner(), func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('shared-repositories:' || $1,0))`, r.InstanceID); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM control.shared_claim_repository WHERE instance_id=$1 AND repo_key<>$2`, r.InstanceID, repo.CanonicalKey()).Scan(&count); err != nil {
			return err
		}
		if count >= maxRepositories {
			return errors.New("shared repository registry is full")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO control.shared_claim_repository(instance_id,repo_key,repository,owner_epoch)
			VALUES($1,$2,$3,$4) ON CONFLICT(instance_id,repo_key) DO UPDATE
			SET repository=excluded.repository,owner_epoch=excluded.owner_epoch,updated_at=clock_timestamp()
			WHERE control.shared_claim_repository.owner_epoch<=$4`, r.InstanceID, repo.CanonicalKey(), raw, epoch)
		if err == nil && tag.RowsAffected() != 1 {
			return lease.ErrStale
		}
		return err
	})
}

func (r Registry) fenced(ctx context.Context, handle *lease.Handle, fn func(context.Context, pgx.Tx, int64) error) error {
	if r.Pool == nil || r.Fence == nil || handle == nil || handle.InstanceID != r.InstanceID {
		return errors.New("shared registry requires its controller lease")
	}
	return r.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx, handle.Epoch) })
}

type registration struct {
	key    string
	repo   providers.RepositoryRef
	cursor string
}

// Reconcile repairs one bounded page per repository under handle. Provider
// failures keep durable custody and the prior cursor, so future epochs retry.
// The writer journals each label write under this same epoch before making it.
func (r Registry) Reconcile(ctx context.Context, handle *lease.Handle, writer providerwriter.SharedVisibilityReconciler) error {
	if writer == nil {
		return errors.New("shared visibility requires the provider writer")
	}
	if err := r.fenced(ctx, handle, func(context.Context, pgx.Tx, int64) error { return nil }); err != nil {
		return err
	}
	rows, err := r.Pool.Query(ctx, `SELECT repo_key,repository,cursor FROM control.shared_claim_repository
		WHERE instance_id=$1 ORDER BY repo_key LIMIT $2`, r.InstanceID, maxRepositories+1)
	if err != nil {
		return err
	}
	repos, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (registration, error) {
		var result registration
		var raw []byte
		if err := row.Scan(&result.key, &raw, &result.cursor); err != nil {
			return result, err
		}
		if err := json.Unmarshal(raw, &result.repo); err != nil {
			return result, err
		}
		if result.repo.Provider != providers.ProviderGitHub || result.key != result.repo.CanonicalKey() {
			return result, errors.New("invalid shared repository registration")
		}
		return result, nil
	})
	if err != nil {
		return err
	}
	if len(repos) > maxRepositories {
		return errors.New("shared repository registry exceeds its bound")
	}
	var failures error
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		itemCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		next, err := writer.ReconcileSharedVisibility(itemCtx, repo.repo, repo.cursor, 64)
		cancel()
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("shared labels for %s: %w", repo.key, err))
			continue
		}
		err = r.fenced(ctx, handle, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
			tag, err := tx.Exec(ctx, `UPDATE control.shared_claim_repository SET cursor=$4,owner_epoch=$5,updated_at=clock_timestamp()
				WHERE instance_id=$1 AND repo_key=$2 AND cursor=$3 AND owner_epoch<=$5`, r.InstanceID, repo.key, repo.cursor, next, epoch)
			if err == nil && tag.RowsAffected() != 1 {
				return lease.ErrStale
			}
			return err
		})
		if err != nil {
			return errors.Join(failures, err)
		}
	}
	return failures
}
