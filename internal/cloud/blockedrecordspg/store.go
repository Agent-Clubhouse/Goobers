// Package blockedrecordspg stores the learned-dependency block records in
// PostgreSQL for the cloud controller's blocked terminal hook.
package blockedrecordspg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/blockedrecords"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/providers"
)

// MaxRecords bounds the records read to find a cycle; more fails the call
// rather than detect against a truncated graph.
const MaxRecords = 100_000

// Store records blocks under the controller-lease fence.
type Store struct {
	Fence *lease.Manager
	Now   func() time.Time
}

// RecordBlock replaces the item's record and returns the dependency cycle it
// closes, if any. The cycle is returned even when persisting fails after
// detection, as parking.State requires.
func (s Store) RecordBlock(ctx context.Context, handle *lease.Handle, b parking.Block) (blockedcycle.Result, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	key := blockedrecords.Key(b.Repository, b.ItemID)
	record := blockedrecords.Record{
		Repository: b.Repository, ItemID: b.ItemID, Blockers: b.Blockers,
		RunID: b.RunID, Stage: b.Stage, Reason: b.Reason, RecordedAt: now().UTC(),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return blockedcycle.Result{}, err
	}
	var cycle blockedcycle.Result
	err = s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO control.blocked_record (instance_id, record_key, record, owner_epoch)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (instance_id, record_key) DO UPDATE
			SET record = EXCLUDED.record, owner_epoch = EXCLUDED.owner_epoch, recorded_at = now()`,
			handle.InstanceID, key, encoded, handle.Epoch); err != nil {
			return fmt.Errorf("record block: %w", err)
		}
		recs, err := load(ctx, tx, handle.InstanceID)
		if err != nil {
			return err
		}
		cycle = blockedrecords.FindCycle(recs, key)
		return nil
	})
	return cycle, err
}

func load(ctx context.Context, tx pgx.Tx, instanceID string) (map[string]blockedrecords.Record, error) {
	rows, err := tx.Query(ctx, `SELECT record_key, record FROM control.blocked_record WHERE instance_id=$1 ORDER BY record_key LIMIT $2`,
		instanceID, MaxRecords+1)
	if err != nil {
		return nil, fmt.Errorf("read blocked records: %w", err)
	}
	defer rows.Close()
	recs := map[string]blockedrecords.Record{}
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var rec blockedrecords.Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("decode blocked record %q: %w", key, err)
		}
		recs[key] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(recs) > MaxRecords {
		return nil, errors.New("blocked records exceed the cycle-detection bound")
	}
	return recs, nil
}

// State is the parking.State of one epoch: records in PostgreSQL, the run's
// claimed items from the claim ledger, and the backlog repository mapping from
// the applied configuration.
type State struct {
	Store   Store
	Handle  *lease.Handle
	Claimed func(runID string) ([]string, error)
	Backlog func(providers.RepositoryRef) providers.RepositoryRef
}

var _ parking.State = State{}

// ClaimedItemIDs returns the item ids runID currently claims.
func (s State) ClaimedItemIDs(runID string) ([]string, error) { return s.Claimed(runID) }

// BacklogRepository maps a code repository to its backlog repository; without a
// mapping it is the identity.
func (s State) BacklogRepository(repo providers.RepositoryRef) providers.RepositoryRef {
	if s.Backlog == nil {
		return repo
	}
	return s.Backlog(repo)
}

// RecordBlock persists b under the epoch's fence.
func (s State) RecordBlock(b parking.Block) (blockedcycle.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	return s.Store.RecordBlock(ctx, s.Handle, b)
}

// recordTimeout bounds one record write (the parking.State seam carries no
// context).
const recordTimeout = 30 * time.Second
