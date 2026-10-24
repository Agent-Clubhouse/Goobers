//go:build cloudintegration

package terminalwatermarkpg_test

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/terminalwatermarkpg"
)

func TestWatermarkMovesForwardOnlyAndOnlyForTheLiveLease(t *testing.T) {
	pool := pgtest.Pool(t, "twpg")
	oldManager := pgtest.Manager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	store := terminalwatermarkpg.Store{Pool: pool, Fence: oldManager}
	if _, ok, err := store.Load(t.Context(), pgtest.InstanceID); err != nil || ok {
		t.Fatalf("fresh Load = %v, %v; want none", ok, err)
	}
	t1 := time.Unix(1_000, 0).UTC()
	if err := store.Advance(t.Context(), old, t1); err != nil {
		t.Fatal(err)
	}
	if err := store.Advance(t.Context(), old, t1.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.Load(t.Context(), pgtest.InstanceID); err != nil || !ok || !got.Equal(t1) {
		t.Fatalf("Load = %v %v %v, want %v (an earlier Advance must not move it back)", got, ok, err, t1)
	}

	newManager := pgtest.Manager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	if err := store.Advance(t.Context(), old, t1.Add(time.Hour)); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale leader Advance = %v, want ErrStale", err)
	}
	next := terminalwatermarkpg.Store{Pool: pool, Fence: newManager}
	t2 := t1.Add(2 * time.Hour)
	if err := next.Advance(t.Context(), successor, t2); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := next.Load(t.Context(), pgtest.InstanceID); !got.Equal(t2) {
		t.Fatalf("Load = %v, want %v", got, t2)
	}
}

// The reviewed column grants are enough for the controller role to run the
// upsert (ON CONFLICT ... EXCLUDED needs SELECT on the excluded columns).
func TestWatermarkUpsertRunsAsTheControllerRole(t *testing.T) {
	pool := pgtest.Pool(t, "twpg")
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), "SET ROLE goobers_controller"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(t.Context(), "RESET ROLE") //nolint:errcheck
	for i := range 2 {
		if _, err := conn.Exec(t.Context(), `
			INSERT INTO control.terminal_watermark (instance_id, closed_through, owner_epoch) VALUES ('i', now(), $1)
			ON CONFLICT (instance_id) DO UPDATE
			SET closed_through = GREATEST(control.terminal_watermark.closed_through, EXCLUDED.closed_through),
			    owner_epoch = EXCLUDED.owner_epoch, updated_at = now()`, i+1); err != nil {
			t.Fatalf("controller upsert %d: %v", i, err)
		}
	}
	if _, err := conn.Exec(t.Context(), "DELETE FROM control.terminal_watermark"); err == nil {
		t.Fatal("controller role may delete the watermark; the grant is wider than the reviewed write path")
	}
}
