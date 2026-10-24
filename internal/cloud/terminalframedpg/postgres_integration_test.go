//go:build cloudintegration

package terminalframedpg_test

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/terminalframedpg"
)

func TestReceiptsSurviveALeaseChangeAndOnlyTheLiveLeaseWrites(t *testing.T) {
	pool := pgtest.Pool(t, "tfpg")
	controller := pgtest.ControllerPool(t, pool) // every store call runs as goobers_controller
	oldManager := pgtest.ControllerManager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	store := terminalframedpg.Store{Pool: controller, Fence: oldManager}
	if framed, err := store.Framed(t.Context(), pgtest.InstanceID, "run-1"); err != nil || framed {
		t.Fatalf("fresh Framed = %v, %v", framed, err)
	}
	for range 2 { // recording twice is a no-op
		if err := store.MarkFramed(t.Context(), old, "run-1"); err != nil {
			t.Fatal(err)
		}
	}
	newManager := pgtest.ControllerManager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	if err := store.MarkFramed(t.Context(), old, "run-2"); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale leader MarkFramed = %v, want ErrStale", err)
	}
	next := terminalframedpg.Store{Pool: controller, Fence: newManager}
	if framed, err := next.Framed(t.Context(), pgtest.InstanceID, "run-1"); err != nil || !framed {
		t.Fatalf("successor does not see the predecessor's receipt: %v, %v", framed, err)
	}
	if framed, _ := next.Framed(t.Context(), pgtest.InstanceID, "run-2"); framed {
		t.Fatal("a stale leader's receipt was recorded")
	}
	if n, err := next.Prune(t.Context(), successor, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("prune of fresh receipts = %d, %v", n, err)
	}
	if n, err := next.Prune(t.Context(), successor, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune of old receipts = %d, %v; want 1", n, err)
	}
}

// The controller role may not rewrite a receipt: the reviewed write path only
// inserts and prunes.
func TestControllerCannotUpdateReceipts(t *testing.T) {
	pool := pgtest.Pool(t, "tfpg")
	controller := pgtest.ControllerPool(t, pool)
	if _, err := controller.Exec(t.Context(), `UPDATE control.terminal_framed SET owner_epoch=9`); err == nil {
		t.Fatal("controller role may UPDATE receipts; the grant is wider than the reviewed write path")
	}
}
