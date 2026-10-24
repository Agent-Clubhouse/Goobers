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

// MaxRecentRuns bounds the runs remembered per item for re-fire detection.
const MaxRecentRuns = 16

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
		// recent_runs keeps the last MaxRecentRuns runs that advanced the streak, so
		// a re-fired frame is recognised even after other runs charged the item.
		_, err := tx.Exec(ctx, `
			INSERT INTO control.failure_streak (instance_id, kind, repo_key, item_id, count, run_id, stage, owner_epoch, recent_runs)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, ARRAY[$6]::text[])
			ON CONFLICT (instance_id, kind, repo_key, item_id) DO UPDATE
			SET count = EXCLUDED.count, run_id = EXCLUDED.run_id, stage = EXCLUDED.stage,
			    owner_epoch = EXCLUDED.owner_epoch, updated_at = now(),
			    recent_runs = (array_remove(control.failure_streak.recent_runs, $6) || ARRAY[$6]::text[])[
			        greatest(1, cardinality(array_remove(control.failure_streak.recent_runs, $6)) + 2 - $9):]`,
			handle.InstanceID, string(kind), repoKey(repo), itemID, count, runID, stage, handle.Epoch, MaxRecentRuns)
		return err
	})
}

// ChargedBy reports whether runID already advanced the item's streak of kind, so
// a frame re-fired by a successor leader does not charge it twice.
func (s Store) ChargedBy(ctx context.Context, instanceID string, kind Kind, repo providers.RepositoryRef, itemID, runID string) (bool, error) {
	var charged bool
	err := s.Pool.QueryRow(ctx, `
		SELECT coalesce($5 = ANY(recent_runs), false) FROM control.failure_streak
		WHERE instance_id=$1 AND kind=$2 AND repo_key=$3 AND item_id=$4`,
		instanceID, string(kind), repoKey(repo), itemID, runID).Scan(&charged)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return charged, err
}
