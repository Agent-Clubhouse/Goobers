//go:build cloudintegration

package terminalreportpg_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/conformance/outbox"
)

func effect(item string, count int) terminalreport.Effect {
	return terminalreport.Effect{
		Kind:       terminalreport.WorkFailure,
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"},
		ItemID:     item, RunID: "run-1", Stage: "ci", Count: count,
	}
}

// controllerPool connects as goobers_controller (SET ROLE), the role the
// control process holds, so adoption exercises the reviewed column grants on
// owner_epoch, lease_token and lease_until rather than the owner's privileges.
func controllerPool(t *testing.T, owner *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := owner.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var current string
	if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&current); err != nil || current != "goobers_controller" {
		t.Fatalf("current_user = %q, %v; want goobers_controller", current, err)
	}
	return pool
}

func claimOne(t *testing.T, store terminalreportpg.Store, handle *lease.Handle) terminalreportpg.Delivery {
	t.Helper()
	deliveries, err := store.Claim(t.Context(), handle, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("claimed %d deliveries, want 1", len(deliveries))
	}
	return deliveries[0]
}

func deliveredGeneration(t *testing.T, pool *pgxpool.Pool, key string) (int64, bool) {
	t.Helper()
	var generation int64
	var delivered bool
	if err := pool.QueryRow(t.Context(), `
		SELECT delivered_generation, delivered_at IS NOT NULL FROM control.terminal_report
		WHERE instance_id=$1 AND effect_key=$2`, pgtest.InstanceID, key,
	).Scan(&generation, &delivered); err != nil {
		t.Fatal(err)
	}
	return generation, delivered
}

// TestTerminalReportOneDeliveryUnderSkew takes the lease over from a holder
// that has an outstanding delivery. The successor's host clock runs an hour
// ahead, but the delivery lease is on the database clock, so the successor
// cannot claim it until it adopts the predecessor's epoch; the predecessor
// can no longer acknowledge after the takeover.
func TestTerminalReportOneDeliveryUnderSkew(t *testing.T) {
	pool := pgtest.Pool(t, "trpg")
	controller := controllerPool(t, pool)
	oldManager := pgtest.Manager(t, controller, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	oldStore := terminalreportpg.Store{Fence: oldManager}
	e := effect("42", 1)
	if _, err := oldStore.Publish(t.Context(), old, e); err != nil {
		t.Fatal(err)
	}
	inFlight := claimOne(t, oldStore, old)

	newManager := pgtest.Manager(t, controller, "new", time.Hour)
	successor := pgtest.Takeover(t, pool, newManager)
	newStore := terminalreportpg.Store{Fence: newManager}
	if deliveries, err := newStore.Claim(t.Context(), successor, 10); err != nil || len(deliveries) != 0 {
		t.Fatalf("successor claimed a live database-clock lease: %d deliveries, err %v", len(deliveries), err)
	}
	if err := oldStore.Delivered(t.Context(), old, inFlight); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale owner acknowledgement = %v, want ErrStale", err)
	}
	if adopted, err := newStore.AdoptAll(t.Context(), successor); err != nil || adopted != 1 {
		t.Fatalf("adopt predecessor = %d, %v; want 1", adopted, err)
	}
	if adopted, err := newStore.AdoptAll(t.Context(), successor); err != nil || adopted != 0 {
		t.Fatalf("adopting again moved %d rows, err %v; want 0", adopted, err)
	}
	// Even a fence-passing acknowledgement of the voided lease is refused.
	inFlight.OwnerEpoch = successor.Epoch
	if err := newStore.Delivered(t.Context(), successor, inFlight); !errors.Is(err, terminalreportpg.ErrLeaseLost) {
		t.Fatalf("voided lease acknowledgement = %v, want ErrLeaseLost", err)
	}
	redelivery := claimOne(t, newStore, successor)
	if redelivery.IdempotencyKey != inFlight.IdempotencyKey {
		t.Fatalf("redelivery key %q, want the same intent key %q", redelivery.IdempotencyKey, inFlight.IdempotencyKey)
	}
	if err := newStore.Delivered(t.Context(), successor, redelivery); err != nil {
		t.Fatal(err)
	}
	if err := oldStore.Delivered(t.Context(), old, inFlight); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale owner acknowledgement after delivery = %v, want ErrStale", err)
	}
	if generation, delivered := deliveredGeneration(t, pool, e.Key()); !delivered || generation != inFlight.Generation {
		t.Fatalf("delivered=%v generation=%d, want generation %d delivered once", delivered, generation, inFlight.Generation)
	}
	if deliveries, err := newStore.Claim(t.Context(), successor, 10); err != nil || len(deliveries) != 0 {
		t.Fatalf("delivered intent was claimed again: %d, %v", len(deliveries), err)
	}
}

func TestOutboxConformance(t *testing.T) {
	outbox.Run(t, func(t *testing.T) outbox.Backend {
		pool := pgtest.Pool(t, "trpg")
		manager := pgtest.Manager(t, pool, "holder", 0)
		handle := pgtest.Acquire(t, manager)
		return outbox.Backend{
			Open: func(*testing.T) terminalreport.Outbox {
				return terminalreportpg.Outbox{Store: terminalreportpg.Store{Fence: manager}, Handle: handle}
			},
			Expire: func(t *testing.T) {
				if _, err := pool.Exec(t.Context(), `
					UPDATE control.terminal_report
					SET retry_at='-infinity', lease_token=NULL, lease_until=NULL
					WHERE delivered_at IS NULL`); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}

func TestTerminalReportConcurrentClaimHasOneWinner(t *testing.T) {
	pool := pgtest.Pool(t, "trpg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := terminalreportpg.Store{Fence: manager}
	if _, err := store.Publish(t.Context(), handle, effect("7", 1)); err != nil {
		t.Fatal(err)
	}
	const claimers = lease.MaxFencedConnections
	results := make(chan []terminalreportpg.Delivery, claimers)
	var wg sync.WaitGroup
	for range claimers {
		wg.Go(func() {
			deliveries, err := store.Claim(t.Context(), handle, 1)
			if err != nil {
				t.Error(err)
			}
			results <- deliveries
		})
	}
	wg.Wait()
	close(results)
	var won []terminalreportpg.Delivery
	for deliveries := range results {
		won = append(won, deliveries...)
	}
	if len(won) != 1 {
		t.Fatalf("%d claimers won, want exactly 1", len(won))
	}
	acks := make(chan error, 2)
	for range 2 {
		wg.Go(func() { acks <- store.Delivered(t.Context(), handle, won[0]) })
	}
	wg.Wait()
	close(acks)
	var succeeded, lost int
	for err := range acks {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, terminalreportpg.ErrLeaseLost):
			lost++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || lost != 1 {
		t.Fatalf("acknowledgements: %d succeeded, %d lost; want 1 and 1", succeeded, lost)
	}
}

// TestTerminalReportRetryResolvesBeforeCapacity fills the outbox, then shows
// an identical retry resolves to its generation, a changed intent supersedes
// in place, and only a new key reports ErrFull.
func TestTerminalReportRetryResolvesBeforeCapacity(t *testing.T) {
	pool := pgtest.Pool(t, "trpg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := terminalreportpg.Store{Fence: manager, MaxPending: 1}
	first := effect("1", 1)
	firstGeneration, err := store.Publish(t.Context(), handle, first)
	if err != nil {
		t.Fatal(err)
	}
	older := claimOne(t, store, handle)
	if err := store.Freeze(t.Context(), handle, older, "rendered"); err != nil {
		t.Fatal(err)
	}
	if generation, err := store.Publish(t.Context(), handle, first); err != nil || generation != firstGeneration {
		t.Fatalf("identical retry = %d, %v; want generation %d", generation, err, firstGeneration)
	}
	if _, err := store.Publish(t.Context(), handle, effect("2", 1)); !errors.Is(err, terminalreportpg.ErrFull) {
		t.Fatalf("new key at capacity = %v, want ErrFull", err)
	}
	secondGeneration, err := store.Publish(t.Context(), handle, effect("1", 2))
	if err != nil || secondGeneration <= firstGeneration {
		t.Fatalf("changed intent = generation %d, %v; want one above %d", secondGeneration, err, firstGeneration)
	}
	if err := store.Freeze(t.Context(), handle, older, "late"); !errors.Is(err, terminalreport.ErrSuperseded) {
		t.Fatalf("freeze of superseded generation = %v", err)
	}
	if err := store.Delivered(t.Context(), handle, older); !errors.Is(err, terminalreport.ErrSuperseded) {
		t.Fatalf("acknowledging superseded generation = %v", err)
	}
	newer := claimOne(t, store, handle)
	if newer.Generation != secondGeneration || newer.Effect.Count != 2 || newer.Body != "" {
		t.Fatalf("newer delivery = generation %d count %d body %q", newer.Generation, newer.Effect.Count, newer.Body)
	}
	if err := store.Delivered(t.Context(), handle, newer); err != nil {
		t.Fatal(err)
	}
	if generation, delivered := deliveredGeneration(t, pool, first.Key()); !delivered || generation != secondGeneration {
		t.Fatalf("delivered=%v generation=%d, want generation %d", delivered, generation, secondGeneration)
	}
}

func TestTerminalReportDeferAndRetentionUseDatabaseClock(t *testing.T) {
	pool := pgtest.Pool(t, "trpg")
	manager := pgtest.Manager(t, pool, "holder", -24*time.Hour)
	handle := pgtest.Acquire(t, manager)
	store := terminalreportpg.Store{Fence: manager, Retention: time.Hour}
	e := effect("9", 1)
	if _, err := store.Publish(t.Context(), handle, e); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Defer(t.Context(), handle, claimOne(t, store, handle), time.Hour); err != nil {
		t.Fatal(err)
	}
	if deliveries, err := store.Claim(t.Context(), handle, 1); err != nil || len(deliveries) != 0 {
		t.Fatalf("deferred intent claimed early: %d, %v", len(deliveries), err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE control.terminal_report SET retry_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	d := claimOne(t, store, handle)
	if d.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", d.Attempts)
	}
	if err := store.Delivered(t.Context(), handle, d); err != nil {
		t.Fatal(err)
	}
	if pruned, err := store.Prune(t.Context(), handle); err != nil || pruned != 0 {
		t.Fatalf("fresh delivered row pruned: %d, %v", pruned, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE control.terminal_report SET delivered_at=now()-interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if pruned, err := store.Prune(t.Context(), handle); err != nil || pruned != 1 {
		t.Fatalf("expired delivered row pruned %d, %v; want 1", pruned, err)
	}
}

func TestTerminalReportMonotonicGuard(t *testing.T) {
	pool := pgtest.Pool(t, "trpg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := terminalreportpg.Store{Fence: manager}
	if _, err := store.Publish(t.Context(), handle, effect("3", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(t.Context(), handle, effect("3", 2)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE control.terminal_report SET generation=1`,
		`UPDATE control.terminal_report SET owner_epoch=owner_epoch-1`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err == nil {
			t.Fatalf("%s moved a cursor backwards", statement)
		}
	}
}

// TestIdempotencyKeyIsNeverReusedAfterPrune: a pruned delivered row must not
// let a later intent for the same projection reuse its key/generation.
func TestIdempotencyKeyIsNeverReusedAfterPrune(t *testing.T) {
	pool := pgtest.Pool(t, "trpg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := terminalreportpg.Store{Fence: manager, Retention: time.Hour}
	if _, err := store.Publish(t.Context(), handle, effect("5", 1)); err != nil {
		t.Fatal(err)
	}
	spent := claimOne(t, store, handle)
	if err := store.Delivered(t.Context(), handle, spent); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE control.terminal_report SET delivered_at=now()-interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if pruned, err := store.Prune(t.Context(), handle); err != nil || pruned != 1 {
		t.Fatalf("prune = %d, %v", pruned, err)
	}
	if _, err := store.Publish(t.Context(), handle, effect("5", 2)); err != nil {
		t.Fatal(err)
	}
	if next := claimOne(t, store, handle); next.IdempotencyKey == spent.IdempotencyKey {
		t.Fatalf("a different intent reused the spent idempotency key %q", spent.IdempotencyKey)
	}
}

// TestAdoptAllReachesEveryOlderEpoch makes two consecutive takeovers with rows
// stranded at epoch 1 and epoch 2, as goobers_controller: AdoptAll moves both,
// voids the delivery lease of the claimed row and
// leaves delivered rows alone.
func TestAdoptAllReachesEveryOlderEpoch(t *testing.T) {
	pool := pgtest.Pool(t, "trpgall")
	controller := controllerPool(t, pool)
	firstManager := pgtest.Manager(t, controller, "first", 0)
	first := pgtest.Acquire(t, firstManager)
	firstStore := terminalreportpg.Store{Fence: firstManager}
	if _, err := firstStore.Publish(t.Context(), first, effect("1", 1)); err != nil {
		t.Fatal(err)
	}
	inFlight := claimOne(t, firstStore, first)
	if _, err := firstStore.Publish(t.Context(), first, effect("2", 1)); err != nil {
		t.Fatal(err)
	}
	delivered := claimOne(t, firstStore, first)
	if err := firstStore.Delivered(t.Context(), first, delivered); err != nil {
		t.Fatal(err)
	}

	secondManager := pgtest.Manager(t, controller, "second", 0)
	second := pgtest.Takeover(t, pool, secondManager)
	secondStore := terminalreportpg.Store{Fence: secondManager}
	if _, err := secondStore.Publish(t.Context(), second, effect("3", 1)); err != nil {
		t.Fatal(err)
	}
	// The second holder crashes before adopting anything.

	thirdManager := pgtest.Manager(t, controller, "third", 0)
	third := pgtest.Takeover(t, pool, thirdManager)
	thirdStore := terminalreportpg.Store{Fence: thirdManager}
	if adopted, err := thirdStore.AdoptAll(t.Context(), third); err != nil || adopted != 2 {
		t.Fatalf("AdoptAll = %d, %v; want the epoch-1 and epoch-2 rows", adopted, err)
	}
	if adopted, err := thirdStore.AdoptAll(t.Context(), third); err != nil || adopted != 0 {
		t.Fatalf("repeated AdoptAll = %d, %v; want 0", adopted, err)
	}
	var owners []int64
	rows, err := pool.Query(t.Context(), `SELECT owner_epoch FROM control.terminal_report
		WHERE instance_id=$1 AND delivered_at IS NULL ORDER BY effect_key`, pgtest.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var epoch int64
		if err := rows.Scan(&epoch); err != nil {
			t.Fatal(err)
		}
		owners = append(owners, epoch)
	}
	rows.Close()
	if len(owners) != 2 || owners[0] != third.Epoch || owners[1] != third.Epoch {
		t.Fatalf("undelivered owners = %v, want both at epoch %d", owners, third.Epoch)
	}
	// The epoch-1 delivery lease was voided: its holder cannot acknowledge, and
	// the live epoch redelivers under the same intent key.
	inFlight.OwnerEpoch = third.Epoch
	if err := thirdStore.Delivered(t.Context(), third, inFlight); !errors.Is(err, terminalreportpg.ErrLeaseLost) {
		t.Fatalf("voided lease acknowledgement = %v, want ErrLeaseLost", err)
	}
	if err := firstStore.Delivered(t.Context(), first, inFlight); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale holder acknowledgement = %v, want ErrStale", err)
	}
	if _, isDelivered := deliveredGeneration(t, pool, effect("2", 1).Key()); !isDelivered {
		t.Fatal("a delivered row was disturbed by adoption")
	}
}
