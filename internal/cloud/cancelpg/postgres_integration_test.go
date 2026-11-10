//go:build cloudintegration

package cancelpg_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/cloud/cancelpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/test/testsupport/conformance/receipts"
)

func TestCancelReceiptRetryResolvesBeforeCapacity(t *testing.T) {
	pool := pgtest.Pool(t, "cancelpg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := cancelpg.Store{Fence: manager, MaxRecords: 1}
	if _, fresh, err := store.Begin(t.Context(), handle, "k1", "alice", []byte("cancel run-1")); err != nil || !fresh {
		t.Fatalf("first begin fresh=%v err=%v", fresh, err)
	}
	if receipt, fresh, err := store.Begin(t.Context(), handle, "k1", "alice", []byte("cancel run-1")); err != nil || fresh || receipt.Complete {
		t.Fatalf("retry at capacity = %+v fresh=%v err=%v; want existing uncertain receipt", receipt, fresh, err)
	}
	if _, _, err := store.Begin(t.Context(), handle, "k1", "alice", []byte("cancel run-2")); !errors.Is(err, cancelreceipt.ErrConflict) {
		t.Fatalf("mismatched payload = %v, want ErrConflict", err)
	}
	if _, _, err := store.Begin(t.Context(), handle, "k1", "bob", []byte("cancel run-1")); !errors.Is(err, cancelreceipt.ErrConflict) {
		t.Fatalf("mismatched actor = %v, want ErrConflict", err)
	}
	if _, _, err := store.Begin(t.Context(), handle, "k2", "alice", []byte("cancel run-2")); !errors.Is(err, cancelreceipt.ErrFull) {
		t.Fatalf("new key at capacity = %v, want ErrFull", err)
	}
	if err := store.Finish(t.Context(), handle, "k1", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(t.Context(), handle, "k1", []byte("ok")); !errors.Is(err, cancelreceipt.ErrTransition) {
		t.Fatalf("identical finish retry = %v, want ErrTransition", err)
	}
	if err := store.Finish(t.Context(), handle, "k1", []byte("other")); !errors.Is(err, cancelreceipt.ErrTransition) {
		t.Fatalf("second outcome = %v, want ErrTransition", err)
	}
	if receipt, fresh, err := store.Begin(t.Context(), handle, "k1", "alice", []byte("cancel run-1")); err != nil || fresh || string(receipt.Result) != "ok" {
		t.Fatalf("completed retry = %+v fresh=%v err=%v", receipt, fresh, err)
	}
}

func TestReceiptsConformance(t *testing.T) {
	receipts.Run(t, func(t *testing.T) receipts.Backend {
		pool := pgtest.Pool(t, "cancelpg")
		manager := pgtest.Manager(t, pool, "holder", 0)
		handle := pgtest.Acquire(t, manager)
		return receipts.Backend{Open: func(*testing.T) cancelreceipt.Receipts {
			return cancelpg.Store{Fence: manager}.Bind(handle)
		}}
	})
}

func TestCancelReceiptConcurrentBeginHasOneWinner(t *testing.T) {
	pool := pgtest.Pool(t, "cancelpg")
	manager := pgtest.Manager(t, pool, "holder", 0)
	handle := pgtest.Acquire(t, manager)
	store := cancelpg.Store{Fence: manager}
	const callers = lease.MaxFencedConnections
	fresh := make(chan bool, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			_, won, err := store.Begin(t.Context(), handle, "same", "alice", []byte("cancel"))
			if err != nil {
				t.Error(err)
			}
			fresh <- won
		})
	}
	wg.Wait()
	close(fresh)
	winners := 0
	for won := range fresh {
		if won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d fresh reservations, want exactly 1", winners)
	}
}

// TestCancelReceiptStaleOwnerAfterTakeover finishes nothing as the stale
// owner after a real lease takeover; the successor resolves the uncertain
// receipt only after adopting the exact predecessor epoch.
func TestCancelReceiptStaleOwnerAfterTakeover(t *testing.T) {
	pool := pgtest.Pool(t, "cancelpg")
	oldManager := pgtest.Manager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	oldStore := cancelpg.Store{Fence: oldManager}
	if _, fresh, err := oldStore.Begin(t.Context(), old, "k", "alice", []byte("cancel")); err != nil || !fresh {
		t.Fatalf("begin fresh=%v err=%v", fresh, err)
	}
	newManager := pgtest.Manager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	newStore := cancelpg.Store{Fence: newManager}
	if err := oldStore.Finish(t.Context(), old, "k", []byte("ok")); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale finish = %v, want ErrStale", err)
	}
	if err := newStore.Finish(t.Context(), successor, "k", []byte("ok")); !errors.Is(err, cancelreceipt.ErrTransition) {
		t.Fatalf("finish before adoption = %v, want ErrTransition", err)
	}
	if adopted, err := newStore.Adopt(t.Context(), successor, old.Epoch+1); err != nil || adopted != 0 {
		t.Fatalf("inexact adoption moved %d rows, err %v", adopted, err)
	}
	if adopted, err := newStore.Adopt(t.Context(), successor, old.Epoch); err != nil || adopted != 1 {
		t.Fatalf("adopt = %d, %v; want 1", adopted, err)
	}
	if err := newStore.Finish(t.Context(), successor, "k", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.cancel_receipt SET owner_epoch=owner_epoch-1"); err == nil {
		t.Fatal("owner epoch moved backwards")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE control.cancel_receipt SET result='other'"); err == nil {
		t.Fatal("recorded result was rewritten")
	}
}

func TestCancelReceiptRetentionUsesDatabaseClock(t *testing.T) {
	pool := pgtest.Pool(t, "cancelpg")
	// The host clock runs far ahead; only the database clock ages receipts.
	manager := pgtest.Manager(t, pool, "holder", 30*24*time.Hour)
	handle := pgtest.Acquire(t, manager)
	store := cancelpg.Store{Fence: manager, Retention: time.Hour}
	for _, key := range []string{"done", "uncertain"} {
		if _, _, err := store.Begin(t.Context(), handle, key, "alice", []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Finish(t.Context(), handle, "done", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM control.cancel_receipt").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, _, err := store.Begin(t.Context(), handle, "next", "alice", []byte("next")); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 3 {
		t.Fatalf("receipts = %d before database-clock expiry, want 3", n)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.cancel_receipt SET finished_at=now()-interval '2 hours' WHERE key='done'`); err == nil {
		t.Fatal("recorded finish time was rewritten")
	}
	// Age the whole database view instead: retention is finished_at < now()-retention.
	store.Retention = time.Microsecond
	time.Sleep(10 * time.Millisecond)
	if _, _, err := store.Begin(t.Context(), handle, "later", "alice", []byte("later")); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 3 {
		t.Fatalf("receipts = %d after expiry, want 3 (finished pruned, uncertain kept)", n)
	}
	if _, fresh, err := store.Begin(t.Context(), handle, "uncertain", "alice", []byte("uncertain")); err != nil || fresh {
		t.Fatalf("uncertain receipt was pruned: fresh=%v err=%v", fresh, err)
	}
}

// TestCancelReceiptSurvivesTwoTakeovers: epoch 1 begins a cancel and dies, and
// epoch 2 dies before adopting. Epoch 3 finds and adopts the receipt.
func TestCancelReceiptSurvivesTwoTakeovers(t *testing.T) {
	pool := pgtest.Pool(t, "cancelpg")
	first := pgtest.Manager(t, pool, "one", 0)
	firstHandle := pgtest.Acquire(t, first)
	if _, _, err := (cancelpg.Store{Fence: first}).Begin(t.Context(), firstHandle, "k", "alice", []byte("cancel")); err != nil {
		t.Fatal(err)
	}
	pgtest.Takeover(t, pool, pgtest.Manager(t, pool, "two", 0))
	third := pgtest.Manager(t, pool, "three", 0)
	thirdHandle := pgtest.Takeover(t, pool, third)
	store := cancelpg.Store{Fence: third}
	epochs, err := store.OrphanedEpochs(t.Context(), thirdHandle)
	if err != nil || len(epochs) != 1 || epochs[0] != firstHandle.Epoch {
		t.Fatalf("orphaned epochs = %v, %v; want [%d]", epochs, err, firstHandle.Epoch)
	}
	if adopted, err := store.AdoptAll(t.Context(), thirdHandle); err != nil || adopted != 1 {
		t.Fatalf("AdoptAll = %d, %v; want 1", adopted, err)
	}
	if err := store.Finish(t.Context(), thirdHandle, "k", []byte("ok")); err != nil {
		t.Fatalf("successor could not resolve the adopted receipt: %v", err)
	}
	if epochs, err := store.OrphanedEpochs(t.Context(), thirdHandle); err != nil || len(epochs) != 0 {
		t.Fatalf("orphaned epochs after adoption = %v, %v", epochs, err)
	}
}

// TestAdoptAllAdoptsEveryOrphanedEpoch: receipts left by two different dead
// epochs are both adopted, not only the newest epoch's.
func TestAdoptAllAdoptsEveryOrphanedEpoch(t *testing.T) {
	pool := pgtest.Pool(t, "cancelpg")
	first := pgtest.Manager(t, pool, "one", 0)
	firstHandle := pgtest.Acquire(t, first)
	if _, _, err := (cancelpg.Store{Fence: first}).Begin(t.Context(), firstHandle, "k1", "alice", []byte("cancel 1")); err != nil {
		t.Fatal(err)
	}
	second := pgtest.Manager(t, pool, "two", 0)
	secondHandle := pgtest.Takeover(t, pool, second)
	if _, _, err := (cancelpg.Store{Fence: second}).Begin(t.Context(), secondHandle, "k2", "alice", []byte("cancel 2")); err != nil {
		t.Fatal(err)
	}
	third := pgtest.Manager(t, pool, "three", 0)
	thirdHandle := pgtest.Takeover(t, pool, third)
	store := cancelpg.Store{Fence: third}
	epochs, err := store.OrphanedEpochs(t.Context(), thirdHandle)
	if err != nil || len(epochs) != 2 || epochs[0] != firstHandle.Epoch || epochs[1] != secondHandle.Epoch {
		t.Fatalf("orphaned epochs = %v, %v; want [%d %d]", epochs, err, firstHandle.Epoch, secondHandle.Epoch)
	}
	if adopted, err := store.AdoptAll(t.Context(), thirdHandle); err != nil || adopted != 2 {
		t.Fatalf("AdoptAll = %d, %v; want 2", adopted, err)
	}
	for _, key := range []string{"k1", "k2"} {
		if err := store.Finish(t.Context(), thirdHandle, key, []byte("ok")); err != nil {
			t.Fatalf("Finish(%s) after AdoptAll: %v", key, err)
		}
	}
}
