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

func TestTwoReplicaPG16Failover(t *testing.T) { testTwoReplicaPG16Failover(t, false) }

func TestTwoReplicaPG16InvoluntaryFailover(t *testing.T) { testTwoReplicaPG16Failover(t, true) }

func testTwoReplicaPG16Failover(t *testing.T, involuntary bool) {
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
	leaseTTL, renew, selfFence := 2*time.Second, 100*time.Millisecond, 800*time.Millisecond
	retry, tick, takeoverBound := 20*time.Millisecond, 20*time.Millisecond, 2*time.Second
	if os.Getenv("GOOBERS_CONTROL_RTO_PRODUCTION") == "1" {
		leaseTTL, renew, selfFence = lease.DefaultTTL, lease.DefaultRenewInterval, lease.DefaultSelfFenceAfter
		retry, tick, takeoverBound = 2*time.Second, time.Second, lease.DefaultTTL+5*time.Second
	}
	if involuntary {
		takeoverBound = leaseTTL + 5*time.Second
	}
	paused := newLeaseDeadlineClock()
	selfFenced := make(chan error, 1)
	var active, restored atomic.Int32
	var unsafeOverlap atomic.Bool
	makeReplica := func(holder string) (*Host, *lease.Manager) {
		leaseConfig := lease.Config{Pool: pool, InstanceID: "control-test", Name: "controller", Holder: holder, TTL: leaseTTL, RenewInterval: renew, SelfFenceAfter: selfFence}
		if involuntary && holder == "first" {
			leaseConfig.Now, leaseConfig.After = paused.Now, paused.After
		}
		manager, err := lease.New(leaseConfig)
		if err != nil {
			t.Fatal(err)
		}
		runtime := Runtime{Pool: pool, InstanceID: "control-test", Lease: manager, Temporal: temporal, TaskQueue: "no-worker-control-test", Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			return engine.RunInput{AcceptanceID: r.ID, Gaggle: "test", WorkflowName: "remote", SelfExecutionDenied: true}, nil
		}, Services: func(context.Context, *lease.Handle) (EpochServices, error) {
			services := testServices()
			// Count live callbacks, independently of Stop and lease Release.
			services.L3 = func(ctx context.Context) error {
				if active.Add(1) != 1 {
					unsafeOverlap.Store(true)
				}
				defer active.Add(-1)
				<-ctx.Done()
				return ctx.Err()
			}
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
		replicaRetry := retry
		if involuntary && holder == "first" {
			replicaRetry = leaseTTL + 10*time.Second
		}
		host, err := New(Config{ValidateAssembly: func() error { return nil }, SelfExecution: "deny", Leadership: manager, SchemaCompatible: func(context.Context) error { return nil }, BuildEpoch: runtime.Build, RetryInterval: replicaRetry, TickInterval: tick, StopTimeout: time.Second, Report: func(err error) {
			if holder == "first" {
				select {
				case selfFenced <- err:
				default:
				}
			}
		}})
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
	waitControl(t, ctx, func() bool { return active.Load() == 1 })
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
	if involuntary {
		// The holder cannot renew. Drive its monotonic watchdog to the deadline;
		// database TTL and the successor still use PostgreSQL's real clock.
		paused.FenceAt(t, ctx, selfFence)
		select {
		case err := <-selfFenced:
			if !errors.Is(err, lease.ErrSelfFenced) {
				t.Fatalf("wrong loss path: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		var kind *string
		if err := pool.QueryRow(ctx, `SELECT release_kind FROM control.lease_history WHERE instance_id='control-test' AND epoch=$1`, old.Epoch).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != nil {
			t.Fatalf("self-fenced holder released voluntarily: %s", *kind)
		}
	}
	stopFirst() // After self-fencing there is no acquired epoch left to release.
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
	if latency := time.Since(started); latency > takeoverBound {
		t.Fatalf("takeover exceeded bound: %v", latency)
	} else {
		t.Logf("IN-PROCESS-HANDOFF: %s", latency)
	}
	waitControl(t, ctx, func() bool { return active.Load() == 1 })
	if involuntary {
		var kind string
		if err := pool.QueryRow(ctx, `SELECT release_kind FROM control.lease_history WHERE instance_id='control-test' AND epoch=$1`, old.Epoch).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != "expired" {
			t.Fatalf("involuntary handoff kind=%q", kind)
		}
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
	wantFence := lease.ErrStale
	if involuntary {
		wantFence = lease.ErrSelfFenced
	}
	if err := observations.Replace(ctx, nil); !errors.Is(err, wantFence) {
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

func TestPGControllerStopsAtSelfFenceDeadline(t *testing.T) {
	pool := controlTestPool(t)
	clock := newLeaseDeadlineClock()
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "loss", Name: "controller", Holder: "old", TTL: 2 * time.Second, RenewInterval: 100 * time.Millisecond, SelfFenceAfter: 800 * time.Millisecond, Now: clock.Now, After: clock.After})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	reported := make(chan error, 1)
	host, err := New(Config{ValidateAssembly: func() error { return nil }, SelfExecution: "deny", Leadership: manager, SchemaCompatible: func(context.Context) error { return nil }, RetryInterval: time.Hour, Report: func(err error) { reported <- err }, BuildEpoch: func(context.Context, *lease.Handle) (EpochServices, error) {
		services := testServices()
		services.Stop = func(context.Context) error { close(stopped); return nil }
		return services, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	waitControl(t, ctx, host.Ready)
	old := host.Owner()
	clock.FenceAt(t, ctx, 800*time.Millisecond)
	select {
	case err := <-reported:
		if !errors.Is(err, lease.ErrSelfFenced) {
			t.Fatalf("wrong deadline path: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	<-stopped
	if host.Ready() || host.Owner() != nil || !manager.SelfFenced(old) {
		t.Fatal("deadline left owner active")
	}
	if err := manager.WithFence(ctx, old, func(context.Context, pgx.Tx) error { t.Error("self-fenced callback entered"); return nil }); !errors.Is(err, lease.ErrSelfFenced) {
		t.Fatalf("deadline fence=%v", err)
	}
	var holder string
	if err := pool.QueryRow(ctx, `SELECT holder FROM control.lease WHERE instance_id='loss' AND name='controller'`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if holder != "old" {
		t.Fatalf("deadline released authority early: %s", holder)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// leaseDeadlineClock pauses renewal scheduling without relying on wall-clock
// assertions. PostgreSQL's heartbeat expiry is deliberately not changed.
type leaseDeadlineClock struct {
	base    time.Time
	elapsed atomic.Int64
	timers  chan chan time.Time
}

func newLeaseDeadlineClock() *leaseDeadlineClock {
	return &leaseDeadlineClock{base: time.Now(), timers: make(chan chan time.Time, 8)}
}
func (c *leaseDeadlineClock) Now() time.Time { return c.base.Add(time.Duration(c.elapsed.Load())) }
func (c *leaseDeadlineClock) After(time.Duration) <-chan time.Time {
	timer := make(chan time.Time, 1)
	c.timers <- timer
	return timer
}
func (c *leaseDeadlineClock) FenceAt(t *testing.T, ctx context.Context, deadline time.Duration) {
	t.Helper()
	select {
	case timer := <-c.timers:
		c.elapsed.Store(int64(deadline))
		timer <- c.Now()
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
