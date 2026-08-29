package readmodel

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The change feed (#1929, design §8.1).
//
// # What this replaces
//
// `internal/httpapi/eventstream.go` discovers change by POLLING THE FILESYSTEM
// on a 100ms ticker, tailing byte offsets, with a 5-second sweep that stats
// every historical run it has ever seen, per-run offset and digest state
// retained for all history, an in-memory event ring, and a random per-process
// session id.
//
// Meanwhile the projection updates on run finish. Two mechanisms with different
// latency, completeness, and failure modes — which is why an in-flight run is
// visible to one and not the other, and why several symptoms that present as
// interface bugs are downstream of there being no single ordered notion of
// change.
//
// This is the single ordered notion: a tail of the `change` table, which the
// projector writes in the SAME transaction as the fact it describes (§6.2). So
// "refetch" can never arrive before "the data is there".
//
// # Why cost is bounded by active work rather than history
//
// The filesystem poller's cost grew with the number of runs that had ever
// existed — it stat'd each one. This reads rows after a cursor, so a quiet
// instance with 40,665 runs does the same work as a quiet instance with ten:
// an indexed seek that returns nothing.

// FeedPosition is a client's place in the change feed.
type FeedPosition struct {
	Cursor  Cursor
	Changes []Change
}

// Feed tails committed changes.
type Feed struct {
	store *Store

	mu      sync.Mutex
	waiters map[chan struct{}]struct{}

	readChanges func(context.Context, uint64, int) ([]Change, error)
}

// NewFeed constructs a feed over a store.
func NewFeed(store *Store) *Feed {
	return &Feed{store: store, readChanges: store.Changes}
}

// Notify wakes anything waiting for new changes.
//
// Called by the projector AFTER its transaction commits. That ordering is the
// whole point: a subscriber woken before the commit would read the feed, find
// nothing, and go back to sleep — and the change it was woken for would then
// wait for the next unrelated commit to be noticed.
func (f *Feed) Notify() {
	f.mu.Lock()
	waiters := f.waiters
	f.waiters = nil
	f.mu.Unlock()
	for waiter := range waiters {
		close(waiter)
	}
}

// wait returns a channel closed on the next Notify, and the operation that
// removes that exact channel from the waiter set.
//
// Only Notify used to clear waiters, so every path out of Since that did not
// end in a wakeup — a query error, rows already available, a cancelled
// context — left an unreachable channel behind. On a quiet instance nothing
// ever notifies, so repeated connect/disconnect grew the set without bound.
// Unregistering under the same mutex keeps the register-before-read ordering
// while bounding the set by the number of live waiters.
func (f *Feed) wait() (<-chan struct{}, func()) {
	waiter := make(chan struct{})
	f.mu.Lock()
	if f.waiters == nil {
		f.waiters = make(map[chan struct{}]struct{})
	}
	f.waiters[waiter] = struct{}{}
	f.mu.Unlock()
	return waiter, func() {
		f.mu.Lock()
		delete(f.waiters, waiter)
		f.mu.Unlock()
	}
}

// Since returns changes after a cursor, blocking until there are some or the
// context ends.
//
// # The three named conditions
//
// A cursor that cannot be resumed is refused with a NAMED condition rather than
// a generic "stale cursor" (§8.2). The names matter because the client's correct
// response differs: `epoch_changed` means take a snapshot, `feed_truncated`
// means take a snapshot, and `schema_changed` means the client's own decoding
// may be wrong. Collapsing them into one code would make a rebuilt store and a
// pruned feed indistinguishable from a client that is simply too old.
func (f *Feed) Since(ctx context.Context, cursor Cursor, limit int) (FeedPosition, error) {
	state, err := f.store.State(ctx)
	if err != nil {
		return FeedPosition{}, err
	}
	if condition := EvaluateCursor(cursor, state); condition != CursorOK {
		return FeedPosition{}, &CursorError{Condition: condition, Cursor: cursor, State: state}
	}

	for {
		// Register BEFORE reading. Registering after would drop a change that
		// commits between the read and the registration — the subscriber would
		// then block until the next unrelated commit, and a quiet instance could
		// sit on a stale view indefinitely.
		waiter, unregister := f.wait()

		changes, err := f.readChanges(ctx, cursor.Seq, limit)
		if err != nil {
			unregister()
			return FeedPosition{}, err
		}
		if len(changes) > 0 {
			unregister()
			next := cursor
			next.Seq = changes[len(changes)-1].Seq
			return FeedPosition{Cursor: next, Changes: changes}, nil
		}

		select {
		case <-ctx.Done():
			unregister()
			return FeedPosition{}, ctx.Err()
		case <-waiter:
		}
	}
}

// Head returns the current end of the feed, for a client with no cursor.
func (f *Feed) Head(ctx context.Context) (Cursor, error) {
	state, err := f.store.State(ctx)
	if err != nil {
		return Cursor{}, err
	}
	seq, err := f.store.LatestChangeSeq(ctx)
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{SchemaVersion: state.SchemaVersion, Epoch: state.Epoch, Seq: seq}, nil
}

// CursorError is a refused cursor, carrying the named condition.
type CursorError struct {
	Condition CursorCondition
	Cursor    Cursor
	State     State
}

func (e *CursorError) Error() string {
	return fmt.Sprintf("readmodel: cursor %s is not usable: %s", e.Cursor, e.Condition)
}

// Code is the stable wire code, which is what the client branches on.
func (e *CursorError) Code() string { return string(e.Condition) }

// HeartbeatInterval is contractual (§8.1).
//
// The client arms a liveness deadline against it (#1711), so changing this
// number changes a guarantee the client depends on rather than a local tuning
// choice — a longer interval makes every client's watchdog fire on a healthy
// stream.
const HeartbeatInterval = 15 * time.Second
