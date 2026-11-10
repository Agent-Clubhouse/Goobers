//go:build cloudintegration

package acceptance

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestAcceptancePruneRequiresLiveControllerAndPreservesUnfinished(t *testing.T) {
	stale, pool := testStore(t, 1)
	now := time.Now().Add(-triggerqueue.ReplayRetention - time.Hour)
	record, _, err := stale.Accept(t.Context(), "expired", "actor", []byte("payload"), now)
	if err != nil {
		t.Fatal(err)
	}
	pending, _, err := stale.Accept(t.Context(), "pending", "actor", []byte("pending"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err := stale.Finish(t.Context(), record.ID, triggerqueue.Rejected, "", "rejected", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	api, err := New(pool, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	retry, repeated, err := api.Accept(t.Context(), "expired", "actor", []byte("payload"), time.Now())
	if err != nil || !repeated || retry.ID != record.ID {
		t.Fatalf("API pruned a receipt: %+v %v %v", retry, repeated, err)
	}
	if err := api.Prune(t.Context()); err == nil {
		t.Fatal("unbound API store pruned")
	}
	if err := stale.manager.Release(t.Context(), stale.handle); err != nil {
		t.Fatal(err)
	}
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "test-instance", Name: stale.handle.Name, Holder: "successor"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Prune(t.Context()); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale prune: %v", err)
	}
	current, err := New(pool, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	current.BindFence(manager, handle)
	if err := current.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Get(t.Context(), record.ID, "actor"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired receipt remains: %v", err)
	}
	if got, err := api.Get(t.Context(), pending.ID, "actor"); err != nil || got.ID != pending.ID {
		t.Fatalf("unfinished receipt pruned: %+v %v", got, err)
	}
}
