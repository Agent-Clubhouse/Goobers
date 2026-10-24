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
	controller := pgtest.ControllerPool(t, pool) // every store call runs as goobers_controller
	oldManager := pgtest.ControllerManager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	store := terminalwatermarkpg.Store{Pool: controller, Fence: oldManager}
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

	newManager := pgtest.ControllerManager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	if err := store.Advance(t.Context(), old, t1.Add(time.Hour)); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale leader Advance = %v, want ErrStale", err)
	}
	next := terminalwatermarkpg.Store{Pool: controller, Fence: newManager}
	t2 := t1.Add(2 * time.Hour)
	if err := next.Advance(t.Context(), successor, t2); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := next.Load(t.Context(), pgtest.InstanceID); !got.Equal(t2) {
		t.Fatalf("Load = %v, want %v", got, t2)
	}
}

// The controller role cannot delete the watermark: the reviewed write path only
// moves it forward.
func TestControllerCannotDeleteTheWatermark(t *testing.T) {
	pool := pgtest.Pool(t, "twpg")
	controller := pgtest.ControllerPool(t, pool)
	if _, err := controller.Exec(t.Context(), "DELETE FROM control.terminal_watermark"); err == nil {
		t.Fatal("controller role may delete the watermark; the grant is wider than the reviewed write path")
	}
}
