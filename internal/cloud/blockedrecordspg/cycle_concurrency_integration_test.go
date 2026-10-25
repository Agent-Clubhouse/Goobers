//go:build cloudintegration

package blockedrecordspg_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/pgtest"
)

// insertBarrier pauses the first writer after INSERT, before its cycle snapshot.
// It lives entirely in the test's pgx tracer, leaving the production path intact.
type insertBarrier struct {
	armed    atomic.Bool
	inserted chan uint32
	release  chan struct{}
}

type insertBarrierKey struct{}

func (b *insertBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO control.blocked_record") && b.armed.CompareAndSwap(true, false) {
		return context.WithValue(ctx, insertBarrierKey{}, true)
	}
	return ctx
}

func (b *insertBarrier) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(insertBarrierKey{}) != true || data.Err != nil {
		return
	}
	b.inserted <- conn.PgConn().PID()
	select {
	case <-b.release:
	case <-ctx.Done():
	}
}

func TestConcurrentOppositeEdgesDeferExactlyOneSeed(t *testing.T) {
	admin := pgtest.Pool(t, "brskew")
	barrier := &insertBarrier{inserted: make(chan uint32, 1), release: make(chan struct{}, 1)}
	barrier.armed.Store(true)
	// Always unblock the tracer before the pool cleanup waits for its connections.
	defer close(barrier.release)
	config := pgtest.ControllerPool(t, admin).Config().Copy()
	config.ConnConfig.Tracer = barrier
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	manager := pgtest.Manager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	store := blockedrecordspg.Store{Fence: manager, Defer: true, RetryDelays: []time.Duration{}}
	// An unavailable provider forces the detected cycle into the durable sweep
	// queue; only the writer closing the cycle should mark its seed.
	store.Closed = (&closedSet{err: context.DeadlineExceeded}).check
	type result struct {
		cycle blockedcycle.Result
		err   error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() {
		cycle, err := store.RecordBlock(t.Context(), handle, block("1", "2"))
		first <- result{cycle, err}
	}()
	var firstPID uint32
	select {
	case firstPID = <-barrier.inserted:
	case got := <-first:
		t.Fatalf("first writer never reached insert barrier: %+v", got)
	case <-time.After(5 * time.Second):
		t.Fatal("first writer never reached insert barrier")
	}
	go func() {
		cycle, err := store.RecordBlock(t.Context(), handle, block("2", "1"))
		second <- result{cycle, err}
	}()
	waitForGraphLock(t, admin, firstPID, handle.InstanceID)
	// Both fenced transactions are now live: the first has inserted its edge
	// and the second must wait before inserting the opposite edge. Without the
	// graph lock the second passes the first's uncommitted edge undetected.
	barrier.release <- struct{}{}
	for _, done := range []chan result{first, second} {
		got := <-done
		if got.err != nil || len(got.cycle.Affected) != 0 {
			t.Fatalf("record: %+v, %v; want no inline escalation", got.cycle, got.err)
		}
	}
	var records, marked, attempts int
	var seed string
	if err := pool.QueryRow(t.Context(), `SELECT count(*), count(verify_after), sum(defer_attempts),
		COALESCE(max(record->>'itemId') FILTER (WHERE verify_after IS NOT NULL), '')
		FROM control.blocked_record WHERE instance_id=$1`, handle.InstanceID).Scan(&records, &marked, &attempts, &seed); err != nil {
		t.Fatal(err)
	}
	if records != 2 || marked != 1 || attempts != 1 || seed != "2" {
		t.Fatalf("records=%d marked=%d attempts=%d seed=%q; want 2, 1, 1, 2", records, marked, attempts, seed)
	}
}

// Observe PostgreSQL's actual lock wait, never assume overlap from a sleep.
func waitForGraphLock(t *testing.T, pool *pgxpool.Pool, blocker uint32, instanceID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks,
			(SELECT hashtextextended('control.blocked_record:' || $2, 0) AS key) AS graph
			WHERE locktype='advisory' AND NOT granted
			AND classid = ((graph.key >> 32) & 4294967295)::oid
			AND objid = (graph.key & 4294967295)::oid AND objsubid = 1
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND $1::int = ANY(pg_blocking_pids(pid)))`, blocker, instanceID).Scan(&waiting)
		if err != nil {
			t.Fatalf("opposite writer never waited for the graph lock: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("opposite writer never waited for the graph lock")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestGraphLockCancellationRollsBackAndCanRetry(t *testing.T) {
	admin := pgtest.Pool(t, "brlock")
	pool := pgtest.ControllerPool(t, admin)
	manager := pgtest.Manager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	store := blockedrecordspg.Store{Fence: manager}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('control.blocked_record:' || $1, 0))`, handle.InstanceID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if cycle, err := store.RecordBlock(ctx, handle, block("1", "2")); err == nil || len(cycle.Affected) != 0 {
		t.Fatalf("blocked record: %+v, %v; want failure without a cycle", cycle, err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM control.blocked_record`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("records after cancelled lock: %d, %v; want none", count, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordBlock(t.Context(), handle, block("1", "2")); err != nil {
		t.Fatalf("retry after lock release: %v", err)
	}
}
