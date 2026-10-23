//go:build cloudintegration

package providerobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/localscheduler"
)

// controllerPool opens sessions that act as goobers_controller, the only role
// the observation store runs as in production.
func controllerPool(t *testing.T, base *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := base.Config()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func epochManager(t *testing.T, pool *pgxpool.Pool, holder string) *lease.Manager {
	t.Helper()
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: pgtest.InstanceID, Name: lease.ControllerName, Holder: holder})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func takeover(t *testing.T, owner *pgxpool.Pool, successor *lease.Manager) *lease.Handle {
	t.Helper()
	if _, err := owner.Exec(t.Context(), `UPDATE control.lease_heartbeat SET expires_at=now()-interval '1 second' WHERE instance_id=$1 AND name=$2`, pgtest.InstanceID, lease.ControllerName); err != nil {
		t.Fatal(err)
	}
	handle, err := successor.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

// Two takeovers, as goobers_controller: each leader's captured quota survives
// the next, a zombie's write after takeover is fenced, and the budget the
// middle leader consumed is not handed back to the third.
func TestQuotaSurvivesTwoTakeoversAndFencesZombies(t *testing.T) {
	owner := pgtest.Pool(t, "providerobs")
	runtime := controllerPool(t, owner)
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	// Epoch 1 leader A observes a window and persists it.
	managerA := epochManager(t, runtime, "a")
	handleA, err := managerA.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stateA := localscheduler.NewProviderQuotaState()
	stateA.Record(apiv1.ProviderGitHub, 50, reset)
	quotaA := Quota{State: stateA}
	storeA := control.Observations{Pool: runtime, InstanceID: pgtest.InstanceID, Lease: managerA, Handle: handleA}
	snapshotA, err := quotaA.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := storeA.Replace(t.Context(), snapshotA); err != nil {
		t.Fatalf("leader A snapshot: %v", err)
	}

	// Takeover 1: B restores A's window, spends 20, persists.
	managerB := epochManager(t, runtime, "b")
	handleB := takeover(t, owner, managerB)
	stateB := localscheduler.NewProviderQuotaState()
	quotaB := Quota{State: stateB}
	storeB := control.Observations{Pool: runtime, InstanceID: pgtest.InstanceID, Lease: managerB, Handle: handleB}
	loaded, err := storeB.Load(t.Context())
	if err != nil || len(loaded) != 1 {
		t.Fatalf("B load = %+v %v", loaded, err)
	}
	if err := quotaB.Restore(t.Context(), loaded); err != nil {
		t.Fatal(err)
	}
	if budget := stateB.ReserveCurrentPolls(apiv1.ProviderGitHub, 20); !budget.Known || budget.Allowed != 20 {
		t.Fatalf("B restored budget = %+v", budget)
	}
	snapshotB, err := quotaB.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := storeB.Replace(t.Context(), snapshotB); err != nil {
		t.Fatalf("leader B snapshot: %v", err)
	}

	// The deposed leader A tries to overwrite with its stale 50: fenced.
	if err := storeA.Replace(t.Context(), snapshotA); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("zombie A write = %v, want lease.ErrStale", err)
	}

	// Takeover 2: C sees B's 30, not A's 50.
	managerC := epochManager(t, runtime, "c")
	handleC := takeover(t, owner, managerC)
	stateC := localscheduler.NewProviderQuotaState()
	storeC := control.Observations{Pool: runtime, InstanceID: pgtest.InstanceID, Lease: managerC, Handle: handleC}
	loaded, err = storeC.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := (Quota{State: stateC}).Restore(t.Context(), loaded); err != nil {
		t.Fatal(err)
	}
	if budget := stateC.ReserveCurrentPolls(apiv1.ProviderGitHub, 100); !budget.Known || budget.Allowed != 30 || !budget.ResetAt.Equal(reset) {
		t.Fatalf("C restored budget = %+v, want 30 remaining in the original window", budget)
	}
	// And B, now deposed too, cannot write either.
	if err := storeB.Replace(t.Context(), snapshotB); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("zombie B write = %v, want lease.ErrStale", err)
	}
}
