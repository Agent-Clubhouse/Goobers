//go:build cloudintegration

package cloudreader

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	"github.com/goobers/goobers/internal/readservice"
)

func controllerRole(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := admin.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The composed control-plane reader over the real PostgreSQL journal and read
// model, as the controller role: unavailable before the first applied
// generation, then serving reads (empty data is an answer, not a 500).
func TestReaderOverPostgresAsTheControllerRole(t *testing.T) {
	admin := pgtest.Pool(t, "cloudreader")
	controller := controllerRole(t, admin)
	manager, err := lease.New(lease.Config{Pool: controller, InstanceID: pgtest.InstanceID, Name: lease.ControllerName, Holder: "reader", TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handle := pgtest.Acquire(t, manager)
	t.Cleanup(func() { _ = manager.Release(context.Background(), handle) })
	backend, err := journalpg.New(journalpg.Config{Pool: controller, InstanceID: pgtest.InstanceID, Lease: manager, LiveOwner: func() *lease.Handle { return handle }})
	if err != nil {
		t.Fatal(err)
	}
	model, err := readmodelpg.NewActiveReader(t.Context(), controller)
	if err != nil {
		t.Fatalf("controller cannot open the active read model: %v", err)
	}
	applied := &appliedSwitch{}
	reader, err := New(Config{Journal: backend, ReadModel: model, Applied: applied, Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := reader.ListRuns(ctx, readservice.RunListOptions{}); !errors.Is(err, readservice.ErrUnavailable) {
		t.Fatalf("runs before apply = %v, want unavailable", err)
	}
	applied.apply("sha256:generation", "cloud")
	if _, err := reader.Health(ctx); err != nil {
		t.Fatalf("health as controller: %v", err)
	}
	runs, err := reader.ListRuns(ctx, readservice.RunListOptions{})
	if err != nil {
		t.Fatalf("runs as controller: %v", err)
	}
	if len(runs.Runs) != 0 {
		t.Fatalf("fresh database listed %d runs", len(runs.Runs))
	}
	if inventory, err := reader.Instance(ctx); err != nil || inventory.Name != "cloud" {
		t.Fatalf("instance as controller = %+v, %v", inventory.Name, err)
	}
	if _, err := reader.GetRun(ctx, "0123456789abcdef0123456789abcdef"); !errors.Is(err, readservice.ErrNotFound) {
		t.Fatalf("unknown run = %v, want not found", err)
	}
	contains := Contains(model)
	if ok, err := contains(ctx, "web", "0123456789abcdef0123456789abcdef"); err != nil || ok {
		t.Fatalf("contains for an unknown run = %v, %v", ok, err)
	}
}
