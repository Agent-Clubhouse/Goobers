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
	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
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
	pgtest.EnsureRoles(t, pool)
	if err = (migrate.Runner{Direct: pool, Catalog: catalog, BackupRef: "control-test", InstanceID: "migration", Holder: "migration"}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestTwoReplicaPG16Failover(t *testing.T) { testTwoReplicaPG16Failover(t, false) }

func TestTwoReplicaPG16InvoluntaryFailover(t *testing.T) { testTwoReplicaPG16Failover(t, true) }

func testTwoReplicaPG16Failover(t *testing.T, involuntary bool) {
	pool := controlTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
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
	temporal := server.Client()
	leaseTTL, renew, selfFence := 2*time.Second, 100*time.Millisecond, 800*time.Millisecond
	retry, tick := 20*time.Millisecond, 20*time.Millisecond
	if os.Getenv("GOOBERS_CONTROL_RTO_PRODUCTION") == "1" {
		leaseTTL, renew, selfFence = lease.DefaultTTL, lease.DefaultRenewInterval, lease.DefaultSelfFenceAfter
		retry, tick = 2*time.Second, time.Second
	}
	selfFenced := make(chan error, 1)
	var active, restored atomic.Int32
	var unsafeOverlap atomic.Bool
	makeReplica := func(holder string) (*Host, *lease.Manager) {
		leaseConfig := lease.Config{Pool: pool, InstanceID: "control-test", Name: lease.ControllerName, Holder: holder, TTL: leaseTTL, RenewInterval: renew, SelfFenceAfter: selfFence}
		manager, err := lease.New(leaseConfig)
		if err != nil {
			t.Fatal(err)
		}
		runtime := Runtime{Configuration: testConfiguration{}, Pool: pool, InstanceID: "control-test", Lease: manager, Temporal: temporal, TaskQueue: "no-worker-control-test", Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
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
		},
			Admission: inertAdmissionFactory}
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
	second, _ := makeReplica("second")
	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Run(secondCtx) }()
	if second.Ready() {
		t.Fatal("standby became ready while first held lease")
	}
	started := time.Now()
	var holderExpiresAt time.Time
	if involuntary {
		// Keep real clocks and successful heartbeats throughout setup. Only now
		// fail the holder's renewal SQL, letting its normal watchdog self-fence.
		holderExpiresAt = failFirstHolderRenewals(t, ctx, pool)
		select {
		case err := <-selfFenced:
			if !errors.Is(err, lease.ErrSelfFenced) {
				t.Fatalf("wrong loss path: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		var renewalFailed bool
		if err := pool.QueryRow(ctx, `SELECT is_called FROM control.failed_renewals`).Scan(&renewalFailed); err != nil {
			t.Fatal(err)
		}
		if !renewalFailed {
			t.Fatal("holder self-fenced without exercising the failed heartbeat path")
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
	// Report latency for the nightly distribution gate; correctness here uses
	// database-clock ordering, not a wall-clock SLO on a loaded test runner.
	t.Logf("IN-PROCESS-HANDOFF: %s", time.Since(started))
	waitControl(t, ctx, func() bool { return active.Load() == 1 })
	if involuntary {
		var kind string
		if err := pool.QueryRow(ctx, `SELECT release_kind FROM control.lease_history WHERE instance_id='control-test' AND epoch=$1`, old.Epoch).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != "expired" {
			t.Fatalf("involuntary handoff kind=%q", kind)
		}
		var successorAcquiredAt time.Time
		if err := pool.QueryRow(ctx, `SELECT acquired_at FROM control.lease_history WHERE instance_id='control-test' AND name=$2 AND epoch=$1`, second.Owner().Epoch, lease.ControllerName).Scan(&successorAcquiredAt); err != nil {
			t.Fatal(err)
		}
		if successorAcquiredAt.Before(holderExpiresAt) {
			t.Fatalf("successor acquired at %s before holder expired at %s", successorAcquiredAt, holderExpiresAt)
		}
	}
	if first.Ready() || first.Owner() != nil || unsafeOverlap.Load() {
		t.Fatal("controllers overlapped authority")
	}
	if restored.Load() == 0 {
		t.Fatal("provider observations were not restored on takeover")
	}
	observations := Observations{Pool: pool, InstanceID: "control-test", Lease: firstManager, Handle: old}
	// The old handle's write must be refused. After an involuntary handoff it
	// is self-fenced. After a voluntary one the new epoch refuses it
	// (ErrStale), unless the handle's own renew window has already lapsed (it
	// stopped renewing at release), in which case WithFence refuses it as
	// self-fenced before it reaches the database: on a loaded host the test
	// gets here after the window. Either is a refusal.
	err = observations.Replace(ctx, nil)
	refused := errors.Is(err, lease.ErrSelfFenced) || (!involuntary && errors.Is(err, lease.ErrStale))
	if !refused {
		t.Fatalf("zombie observation write: %v", err)
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

// The database is private to this test. A sequence records failed attempts even
// though the renewal transaction rolls back; takeover and release remain usable.
func failFirstHolderRenewals(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Time {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		CREATE SEQUENCE control.failed_renewals;
		CREATE FUNCTION control.fail_first_renewal() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM nextval('control.failed_renewals');
			RAISE EXCEPTION 'injected first-holder heartbeat failure';
		END;
		$$;
		CREATE TRIGGER fail_first_renewal BEFORE UPDATE OF expires_at ON control.lease_heartbeat
		FOR EACH ROW WHEN (NEW.holder = 'first' AND NEW.holder = OLD.holder AND NEW.epoch = OLD.epoch)
		EXECUTE FUNCTION control.fail_first_renewal();
	`)
	if err != nil {
		t.Fatal(err)
	}
	// CREATE TRIGGER retains its table lock until commit, so capture the last
	// successful heartbeat before a successor can replace the row.
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `SELECT expires_at FROM control.lease_heartbeat WHERE instance_id='control-test' AND name=$1 AND holder='first'`, lease.ControllerName).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return expiresAt
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
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "loss", Name: lease.ControllerName, Holder: "old", TTL: 2 * time.Second, RenewInterval: 100 * time.Millisecond, SelfFenceAfter: 800 * time.Millisecond, Now: clock.Now, After: clock.After})
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
	if err := pool.QueryRow(ctx, `SELECT holder FROM control.lease WHERE instance_id='loss' AND name=$1`, lease.ControllerName).Scan(&holder); err != nil {
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

type testConfiguration struct{}

func (testConfiguration) Admit(string, string, string) error { return nil }
func (testConfiguration) Reload(context.Context) error       { return nil }
