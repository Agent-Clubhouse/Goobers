// Package failurestreakpg stores the circuit breaker's consecutive-failure
// counters in PostgreSQL for the cloud controller's failed and escalated
// terminal hooks.
package failurestreakpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/providers"
)

// Kind separates the genuine/work streak from the infrastructure streak.
type Kind string

const (
	// Work is the genuine/work failure streak.
	Work Kind = "work"
	// Infra is the item-correlated infrastructure streak (#5638).
	Infra Kind = "infra"
)

// Store is the streak table for one instance.
type Store struct {
	Pool  *pgxpool.Pool
	Fence *lease.Manager
}

func repoKey(repo providers.RepositoryRef) string { return blockedcycle.RepositoryIdentity(repo) }

// Load returns an item's streak count; none is zero.
func (s Store) Load(ctx context.Context, instanceID string, kind Kind, repo providers.RepositoryRef, itemID string) (int, error) {
	var count int
	err := s.Pool.QueryRow(ctx,
		`SELECT count FROM control.failure_streak WHERE instance_id=$1 AND kind=$2 AND repo_key=$3 AND item_id=$4`,
		instanceID, string(kind), repoKey(repo), itemID).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return count, err
}

// Write sets an item's streak under the controller-lease fence; zero resets it
// (drops the row).
func (s Store) Write(ctx context.Context, handle *lease.Handle, kind Kind, repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		if count <= 0 {
			_, err := tx.Exec(ctx,
				`DELETE FROM control.failure_streak WHERE instance_id=$1 AND kind=$2 AND repo_key=$3 AND item_id=$4`,
				handle.InstanceID, string(kind), repoKey(repo), itemID)
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO control.failure_streak (instance_id, kind, repo_key, item_id, count, run_id, stage, owner_epoch)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (instance_id, kind, repo_key, item_id) DO UPDATE
			SET count = EXCLUDED.count, run_id = EXCLUDED.run_id, stage = EXCLUDED.stage,
			    owner_epoch = EXCLUDED.owner_epoch, updated_at = now()`,
			handle.InstanceID, string(kind), repoKey(repo), itemID, count, runID, stage, handle.Epoch)
		return err
	})
}
