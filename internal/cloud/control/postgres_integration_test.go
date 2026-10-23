//go:build cloudintegration

package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func controlTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("control_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err = (migrate.Runner{Direct: pool, Catalog: catalog, BackupRef: "control-test", InstanceID: "migration", Holder: "migration"}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool
}

// lostResponseClient commits the first Temporal start but hides its response.
// No worker is launched: control must never execute the queued workflow stages.
type lostResponseClient struct {
	client.Client
	starts atomic.Int32
	lost   atomic.Bool
}

func (c *lostResponseClient) ExecuteWorkflow(ctx context.Context, o client.StartWorkflowOptions, w interface{}, args ...interface{}) (client.WorkflowRun, error) {
	run, err := c.Client.ExecuteWorkflow(ctx, o, w, args...)
	if err == nil {
		c.starts.Add(1)
		if c.lost.CompareAndSwap(false, true) {
			return nil, errors.New("lost start response")
		}
	}
	return run, err
}

func TestTwoReplicaPG16Failover(t *testing.T) {
	pool := controlTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	temporal := &lostResponseClient{Client: server.Client()}
	var active, restored atomic.Int32
	var unsafeOverlap atomic.Bool
	makeReplica := func(holder string) (*Host, *lease.Manager) {
		manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "control-test", Name: "controller", Holder: holder, TTL: 2 * time.Second, RenewInterval: 100 * time.Millisecond, SelfFenceAfter: 800 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		runtime := Runtime{Pool: pool, InstanceID: "control-test", Lease: manager, Temporal: temporal, TaskQueue: "no-worker-control-test", Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			return engine.RunInput{AcceptanceID: r.ID, Gaggle: "test", WorkflowName: "remote", SelfExecutionDenied: true}, nil
		}, Services: func(context.Context, *lease.Handle) (EpochServices, error) {
			services := testServices()
			services.RebuildBeforeReap = func(context.Context) error {
				if active.Add(1) != 1 {
					unsafeOverlap.Store(true)
				}
				return nil
			}
			services.Stop = func(context.Context) error { active.Add(-1); return nil }
			services.RestoreObservations = func(_ context.Context, rows []Observation) error {
				if len(rows) == 1 && strings.Contains(string(rows[0].Payload), "openPRs") {
					restored.Add(1)
				}
				return nil
			}
			services.CaptureObservations = func(context.Context) ([]Observation, error) {
				return []Observation{{Key: "provider/quota", Payload: []byte(`{"openPRs":3,"remaining":19}`), ObservedAt: time.Now()}}, nil
			}
			return services, nil
		}}
		host, err := New(Config{SelfExecution: "deny", Leadership: manager, SchemaCompatible: func(context.Context) error { return nil }, BuildEpoch: runtime.Build, RetryInterval: 20 * time.Millisecond, TickInterval: 20 * time.Millisecond, StopTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return host, manager
	}
	first, firstManager := makeReplica("first")
	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Run(firstCtx) }()
	waitControl(t, ctx, first.Ready)
	old := first.Owner()
	store, err := acceptance.New(pool, "control-test")
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(firstManager, old)
	record, _, err := store.Accept(ctx, "request/1", "operator", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	waitControl(t, ctx, func() bool { return temporal.lost.Load() })
	second, _ := makeReplica("second")
	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Run(secondCtx) }()
	if second.Ready() {
		t.Fatal("standby became ready while first held lease")
	}
	started := time.Now()
	stopFirst() // SIGTERM uses this same context-cancellation path.
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitControl(t, ctx, second.Ready)
	waitControl(t, ctx, func() bool {
		r, err := store.Get(ctx, record.ID, "operator")
		return err == nil && r.State == triggerqueue.Dispatched
	})
	if latency := time.Since(started); latency > 2*time.Second {
		t.Fatalf("takeover exceeded bound: %v", latency)
	} else {
		t.Logf("RESTART-RTO: %s", latency)
	}
	if first.Ready() || first.Owner() != nil || unsafeOverlap.Load() {
		t.Fatal("controllers overlapped authority")
	}
	if temporal.starts.Load() != 1 {
		t.Fatalf("duplicate starts: %d", temporal.starts.Load())
	}
	if restored.Load() == 0 {
		t.Fatal("provider observations were not restored on takeover")
	}
	observations := Observations{Pool: pool, InstanceID: "control-test", Lease: firstManager, Handle: old}
	if err := observations.Replace(ctx, nil); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("zombie observation write: %v", err)
	}
	runID := strings.TrimPrefix(record.ID, "trigger-")
	description, err := temporal.DescribeWorkflowExecution(ctx, runID, "")
	if err != nil || description.WorkflowExecutionInfo.GetStatus().String() != "Running" {
		t.Fatalf("control shutdown stopped engine workflow: %v %v", description, err)
	}
	stopSecond()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitControl(t *testing.T, ctx context.Context, predicate func() bool) {
	t.Helper()
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
		}
	}
}

func TestPGControllerStopsWithinSelfFenceBoundAfterLeaseLoss(t *testing.T) {
	pool := controlTestPool(t)
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "loss", Name: "controller", Holder: "old", TTL: 2 * time.Second, RenewInterval: 100 * time.Millisecond, SelfFenceAfter: 800 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	host, err := New(Config{SelfExecution: "deny", Leadership: manager, SchemaCompatible: func(context.Context) error { return nil }, RetryInterval: 10 * time.Second, TickInterval: 10 * time.Millisecond, BuildEpoch: func(context.Context, *lease.Handle) (EpochServices, error) {
		services := testServices()
		services.Stop = func(context.Context) error { close(stopped); return nil }
		return services, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	waitControl(t, ctx, host.Ready)
	old := host.Owner()
	start := time.Now()
	if err := manager.Release(ctx, old); err != nil {
		t.Fatal(err)
	}
	successor, err := lease.New(lease.Config{Pool: pool, InstanceID: "loss", Name: "controller", Holder: "new"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := successor.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = successor.Release(context.Background(), next) }()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if host.Ready() || host.Owner() != nil || time.Since(start) > 800*time.Millisecond {
		t.Fatal("lost owner remained active beyond self-fence bound")
	}
	if err := manager.WithFence(ctx, old, func(context.Context, pgx.Tx) error { t.Error("stale callback entered"); return nil }); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale write=%v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
