// Package blockedrecordspg stores the learned-dependency block records in
// PostgreSQL for the cloud controller's blocked terminal hook.
package blockedrecordspg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// ClosedFunc reports whether an item is VERIFIED closed on the provider. Only a
// definite "closed" is true; an error means unknown and the record is kept.
// runID is the run whose record names the item (the writer resolves the
// repository's credentials from that run's pinned configuration).
type ClosedFunc func(ctx context.Context, runID string, repo providers.RepositoryRef, itemID string) (bool, error)

// Store records blocks under the controller-lease fence.
type Store struct {
	Fence *lease.Manager
	Now   func() time.Time
	// Closed verifies closure before a cycle is acted on and during Heal. Without
	// it no record is ever dropped, so stale records can accumulate.
	Closed ClosedFunc
}

// RecordBlock replaces the item's record and returns the dependency cycle it
// closes, if any. The cycle is returned even when persisting fails after
// detection, as parking.State requires.
func (s Store) RecordBlock(ctx context.Context, handle *lease.Handle, b parking.Block) (blockedcycle.Result, error) {
	cycle, err := s.record(ctx, handle, b)
	if err != nil || len(cycle.Affected) == 0 || s.Closed == nil {
		return cycle, err
	}
	// A cycle through a closed item is false: a stale record would escalate
	// non-cyclic items needs-human. Verify the members outside any transaction,
	// forget the closed ones and look again, until the cycle is real or gone.
	key := blockedrecords.Key(b.Repository, b.ItemID)
	for range cycle.Affected {
		recs, serr := s.snapshot(ctx, handle)
		if serr != nil {
			return cycle, serr
		}
		closed := s.closedNodes(ctx, recs, cycle.Affected, b.RunID)
		if len(closed) == 0 {
			return cycle, nil
		}
		if ferr := s.Forget(ctx, handle, closed); ferr != nil {
			return cycle, ferr
		}
		recs, serr = s.snapshot(ctx, handle)
		if serr != nil {
			return cycle, serr
		}
		if cycle = blockedrecords.FindCycle(recs, key); len(cycle.Affected) == 0 {
			return cycle, nil
		}
	}
	return cycle, nil
}

func (s Store) record(ctx context.Context, handle *lease.Handle, b parking.Block) (blockedcycle.Result, error) {
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

// snapshot reads every record under the fence.
func (s Store) snapshot(ctx context.Context, handle *lease.Handle) (map[string]blockedrecords.Record, error) {
	var recs map[string]blockedrecords.Record
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recs, err = load(ctx, tx, handle.InstanceID)
		return err
	})
	return recs, err
}

// runIDs maps every node a record mentions (the item and its blockers) to the
// run that recorded it.
func runIDs(recs map[string]blockedrecords.Record) map[blockedcycle.Node]string {
	nodes := map[blockedcycle.Node]string{}
	for key, rec := range recs {
		if blockedrecords.RepositoryEmpty(rec.Repository) {
			continue
		}
		nodes[blockedcycle.Node{Repository: rec.Repository, ItemID: blockedrecords.LookupID(blockedrecords.ItemID(key, rec))}] = rec.RunID
		for _, blocker := range rec.Blockers {
			node := blockedcycle.Node{Repository: rec.Repository, ItemID: blockedrecords.LookupID(blocker)}
			if _, ok := nodes[node]; !ok {
				nodes[node] = rec.RunID
			}
		}
	}
	return nodes
}

// closedNodes returns the candidates verified closed. An unverifiable one is
// kept: a record is only dropped on positive evidence.
func (s Store) closedNodes(ctx context.Context, recs map[string]blockedrecords.Record, candidates []blockedcycle.Node, fallbackRun string) []blockedcycle.Node {
	runs := runIDs(recs)
	var closed []blockedcycle.Node
	for _, node := range candidates {
		run := runs[node]
		if run == "" {
			run = fallbackRun
		}
		if done, err := s.Closed(ctx, run, node.Repository, node.ItemID); err == nil && done {
			closed = append(closed, node)
		}
	}
	return closed
}

// Forget drops what a closed item leaves behind: its own record, and its
// appearance as a blocker in every other record (a record left with no blockers
// is dropped). Fenced.
func (s Store) Forget(ctx context.Context, handle *lease.Handle, closed []blockedcycle.Node) error {
	if len(closed) == 0 {
		return nil
	}
	gone := map[blockedcycle.Node]bool{}
	for _, node := range closed {
		gone[node] = true
	}
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		recs, err := load(ctx, tx, handle.InstanceID)
		if err != nil {
			return err
		}
		for key, rec := range recs {
			if blockedrecords.RepositoryEmpty(rec.Repository) {
				continue
			}
			self := blockedcycle.Node{Repository: rec.Repository, ItemID: blockedrecords.LookupID(blockedrecords.ItemID(key, rec))}
			kept := make([]string, 0, len(rec.Blockers))
			for _, blocker := range rec.Blockers {
				if !gone[blockedcycle.Node{Repository: rec.Repository, ItemID: blockedrecords.LookupID(blocker)}] {
					kept = append(kept, blocker)
				}
			}
			switch {
			case gone[self] || len(kept) == 0:
				if _, err := tx.Exec(ctx, `DELETE FROM control.blocked_record WHERE instance_id=$1 AND record_key=$2`, handle.InstanceID, key); err != nil {
					return fmt.Errorf("forget blocked record: %w", err)
				}
			case len(kept) != len(rec.Blockers):
				rec.Blockers = kept
				encoded, err := json.Marshal(rec)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE control.blocked_record SET record=$3, owner_epoch=$4 WHERE instance_id=$1 AND record_key=$2`,
					handle.InstanceID, key, encoded, handle.Epoch); err != nil {
					return fmt.Errorf("trim blocked record: %w", err)
				}
			}
		}
		return nil
	})
}

// Heal is the self-heal sweep (the cloud counterpart of backlog-query pruning):
// it verifies up to limit distinct items the records mention and forgets the
// closed ones, so the records stay bounded and cannot close false cycles. It
// returns how many items it forgot. A no-op without Closed.
func (s Store) Heal(ctx context.Context, handle *lease.Handle, limit int) (int, error) {
	if s.Closed == nil || limit <= 0 {
		return 0, nil
	}
	recs, err := s.snapshot(ctx, handle)
	if err != nil {
		return 0, err
	}
	nodes := make([]blockedcycle.Node, 0, len(runIDs(recs)))
	for node := range runIDs(recs) {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return blockedrecords.Key(nodes[i].Repository, nodes[i].ItemID) < blockedrecords.Key(nodes[j].Repository, nodes[j].ItemID)
	})
	if len(nodes) > limit {
		// Rotate the window each minute so no item is starved by the ones before it.
		now := time.Now
		if s.Now != nil {
			now = s.Now
		}
		start := int(now().Unix()/60) % len(nodes)
		nodes = append(nodes[start:], nodes[:start]...)[:limit]
	}
	closed := s.closedNodes(ctx, recs, nodes, "")
	return len(closed), s.Forget(ctx, handle, closed)
}
