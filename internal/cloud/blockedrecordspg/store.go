// Package blockedrecordspg stores the learned-dependency block records in
// PostgreSQL for the cloud controller's blocked terminal hook.
package blockedrecordspg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
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

// ErrVerificationDeferred means a cycle's members could not all be verified
// because the provider read budget was spent. The block is recorded, but no cycle
// is reported: escalating needs-human on an unverified cycle could stop work on
// items that are no longer blocked. The cycle is looked at again the next time any
// member's block is recorded.
var ErrVerificationDeferred = errors.New("blocked-record cycle verification deferred: provider read budget spent")

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
	// Reserve accounts one provider read of the given provider in the shared
	// quota ledger and reports whether it may be made now; false skips the
	// verification (the record is kept and tried again later). Nil allows all.
	Reserve func(providers.ProviderKind) bool
	// Backoff keeps an item that cannot be verified from being retried on every
	// pass. Nil retries every time.
	Backoff *Backoff
}

// Backoff delays re-verifying an item whose last verification failed, doubling
// from base up to max, so a record that can never be verified (its repository is
// no longer configured, its credentials are gone) neither burns provider reads
// nor occupies heal slots forever. It is safe for concurrent use.
type Backoff struct {
	Base, Max time.Duration
	Now       func() time.Time

	mu    sync.Mutex
	state map[blockedcycle.Node]backoffState
}

type backoffState struct {
	failures int
	// first is when the item first failed verification, kept until it verifies.
	first time.Time
	until time.Time
}

func (b *Backoff) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Allowed reports whether node may be verified now.
func (b *Backoff) Allowed(node blockedcycle.Node) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.now().Before(b.state[node].until)
}

// Failed records a failed verification of node.
func (b *Backoff) Failed(node blockedcycle.Node) {
	if b == nil {
		return
	}
	base, limit := b.Base, b.Max
	if base <= 0 {
		base = 10 * time.Minute
	}
	if limit <= 0 {
		limit = 6 * time.Hour
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == nil {
		b.state = map[blockedcycle.Node]backoffState{}
	}
	st := b.state[node]
	st.failures++
	if st.first.IsZero() {
		st.first = b.now()
	}
	delay := base
	for i := 1; i < st.failures && delay < limit; i++ {
		delay *= 2
	}
	st.until = b.now().Add(min(delay, limit))
	b.state[node] = st
}

// Failures reports how many times node has failed verification since it last
// verified, and when it first failed.
func (b *Backoff) Failures(node blockedcycle.Node) (attempts int, first time.Time) {
	if b == nil {
		return 0, time.Time{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[node]
	return st.failures, st.first
}

// Retain drops the state of every node not in live, so entries for items that no
// longer appear in any record do not accumulate.
func (b *Backoff) Retain(live map[blockedcycle.Node]bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for node := range b.state {
		if !live[node] {
			delete(b.state, node)
		}
	}
}

// Verified clears node's failures.
func (b *Backoff) Verified(node blockedcycle.Node) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, node)
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
	// forget the closed ones and look again, until the cycle is real or gone. This
	// path does not honour the heal backoff (a block is being decided now), but it
	// does account its reads; if the read budget is spent before every member is
	// verified, nothing is escalated yet.
	key := blockedrecords.Key(b.Repository, b.ItemID)
	// The loop is bounded by the cycle's size, so it normally ends by the cycle
	// disappearing or a pass with nothing to forget. If the bound is ever reached
	// after a pass that closed some members and left others unchecked, the last
	// pass's deferral and unverifiable members decide the answer: the cycle is
	// never handed back as if it were verified.
	var deferred bool
	var lastUnverified []unverified
	for range cycle.Affected {
		recs, serr := s.snapshot(ctx, handle)
		if serr != nil {
			return cycle, serr
		}
		closed, passDeferred, unverified := s.closedNodes(ctx, recs, cycle.Affected, b.RunID, false)
		deferred, lastUnverified = passDeferred, unverified
		if len(closed) == 0 {
			return s.decide(cycle, passDeferred, unverified)
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
	return s.decide(cycle, deferred, lastUnverified)
}

// decide turns a verification pass into the answer RecordBlock gives. A cycle
// whose read budget ran out is deferred, never escalated unverified. A member the
// provider could not answer is held (deferred) while its failure may be transient,
// until it has failed at least MinAttempts times over UnverifiedHold; after that, or
// at once for a failure that can never succeed, the cycle is escalated with the
// unverifiable members named in its comment, so a human knows what was not checked.
func (s Store) decide(cycle blockedcycle.Result, deferred bool, unv []unverified) (blockedcycle.Result, error) {
	if deferred {
		return blockedcycle.Result{}, ErrVerificationDeferred
	}
	var notes []string
	for _, u := range unv {
		if !u.Permanent && !s.ripe(u.Node) {
			return blockedcycle.Result{}, ErrVerificationDeferred
		}
		notes = append(notes, "#"+u.Node.ItemID+": unverifiable: "+u.Class)
	}
	cycle.Unverified = notes
	return cycle, nil
}

// ripe reports whether a retryable failure has been held long enough. Without a
// backoff to track it there is nothing to wait on.
func (s Store) ripe(node blockedcycle.Node) bool {
	if s.Backoff == nil {
		return true
	}
	attempts, first := s.Backoff.Failures(node)
	return attempts >= MinAttempts && !first.IsZero() && s.Backoff.now().Sub(first) >= UnverifiedHold
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

// closedNodes returns the candidates verified closed, whether any candidate went
// unchecked because its provider's read budget was spent, and the candidates whose
// read failed (with why). An unverifiable
// candidate is kept: a record is only dropped on positive evidence. With
// honourBackoff, an item that recently failed verification is skipped (the heal
// sweep); the outcome of every check updates the backoff either way. A provider
// whose budget is spent is skipped for the rest of the pass, but other providers'
// items are still checked.
func (s Store) closedNodes(ctx context.Context, recs map[string]blockedrecords.Record, candidates []blockedcycle.Node, fallbackRun string, honourBackoff bool) (closed []blockedcycle.Node, deferred bool, unverifiable []unverified) {
	runs := runIDs(recs)
	spent := map[providers.ProviderKind]bool{}
	for _, node := range candidates {
		if honourBackoff && !s.Backoff.Allowed(node) {
			continue
		}
		provider := node.Repository.Provider
		if spent[provider] {
			deferred = true
			continue
		}
		if s.Reserve != nil && !s.Reserve(provider) {
			spent[provider] = true
			deferred = true
			continue
		}
		run := runs[node]
		if run == "" {
			run = fallbackRun
		}
		done, err := s.Closed(ctx, run, node.Repository, node.ItemID)
		if err != nil {
			s.Backoff.Failed(node)
			class, permanent := classify(err)
			unverifiable = append(unverifiable, unverified{Node: node, Class: class, Permanent: permanent})
			continue
		}
		s.Backoff.Verified(node)
		if done {
			closed = append(closed, node)
		}
	}
	return closed, deferred, unverifiable
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
		s.Backoff.Verified(node) // a forgotten item needs no backoff entry
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
	live := make(map[blockedcycle.Node]bool, len(nodes))
	for _, node := range nodes {
		live[node] = true
	}
	s.Backoff.Retain(live)
	// Items in backoff take no slot: an unverifiable item must not occupy the
	// window and starve the ones that can be checked.
	nodes = readyNodes(nodes, s.Backoff)
	if len(nodes) > limit {
		// Rotate the window each minute so no item is starved by the ones before it.
		now := time.Now
		if s.Now != nil {
			now = s.Now
		}
		start := int(now().Unix()/60) % len(nodes)
		nodes = append(nodes[start:], nodes[:start]...)[:limit]
	}
	closed, _, _ := s.closedNodes(ctx, recs, nodes, "", true)
	return len(closed), s.Forget(ctx, handle, closed)
}

// readyNodes drops the nodes still in backoff, preserving order.
func readyNodes(nodes []blockedcycle.Node, backoff *Backoff) []blockedcycle.Node {
	ready := nodes[:0:0]
	for _, node := range nodes {
		if backoff.Allowed(node) {
			ready = append(ready, node)
		}
	}
	return ready
}
