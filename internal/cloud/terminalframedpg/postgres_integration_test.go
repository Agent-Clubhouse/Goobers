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
	oldManager := pgtest.Manager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	store := terminalframedpg.Store{Pool: pool, Fence: oldManager}
	if framed, err := store.Framed(t.Context(), pgtest.InstanceID, "run-1"); err != nil || framed {
		t.Fatalf("fresh Framed = %v, %v", framed, err)
	}
	for range 2 { // recording twice is a no-op
		if err := store.MarkFramed(t.Context(), old, "run-1"); err != nil {
			t.Fatal(err)
		}
	}
	newManager := pgtest.Manager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	if err := store.MarkFramed(t.Context(), old, "run-2"); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale leader MarkFramed = %v, want ErrStale", err)
	}
	next := terminalframedpg.Store{Pool: pool, Fence: newManager}
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

func TestReceiptWritesRunAsTheControllerRole(t *testing.T) {
	pool := pgtest.Pool(t, "tfpg")
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), "SET ROLE goobers_controller"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(t.Context(), "RESET ROLE") //nolint:errcheck
	for range 2 {
		if _, err := conn.Exec(t.Context(), `
			INSERT INTO control.terminal_framed (instance_id, run_id, owner_epoch) VALUES ('i', 'r', 1)
			ON CONFLICT (instance_id, run_id) DO NOTHING`); err != nil {
			t.Fatalf("controller insert: %v", err)
		}
	}
	if _, err := conn.Exec(t.Context(), `DELETE FROM control.terminal_framed WHERE instance_id='i' AND framed_at < now()`); err != nil {
		t.Fatalf("controller prune: %v", err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE control.terminal_framed SET owner_epoch=9`); err == nil {
		t.Fatal("controller role may UPDATE receipts; the grant is wider than the reviewed write path")
	}
}
