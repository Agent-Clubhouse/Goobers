//go:build cloudintegration

// Regression cases from the adversarial HA-P3-18 lifecycle review.
package control

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// A: control.go wires an incomplete assembly as a BuildEpoch error. The host
// still acquires the controller lease, fails, releases, and retries forever:
// every cycle bumps the epoch twice and appends a lease_history row.
func TestReviewIncompleteAssemblyNeverAcquiresAuthority(t *testing.T) {
	pool := controlTestPool(t)
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "churn", Name: lease.ControllerName, Holder: "incomplete"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := New(Config{ValidateAssembly: func() error { return errors.New("missing PG planes") }, SelfExecution: "deny", Leadership: manager, SchemaCompatible: func(context.Context) error { return nil }, BuildEpoch: func(context.Context, *lease.Handle) (EpochServices, error) {
		t.Fatal("incomplete assembly built epoch")
		return EpochServices{}, nil
	}})
	if err == nil || host != nil {
		t.Fatalf("incomplete assembly accepted: %v %v", host, err)
	}
	var leases, history int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM control.lease WHERE instance_id='churn'`).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM control.lease_history WHERE instance_id='churn'`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if leases != 0 || history != 0 {
		t.Fatalf("incomplete assembly created %d leases, %d history rows", leases, history)
	}
}

type countingTemporal struct {
	client.Client
	starts atomic.Int32
}

func (c *countingTemporal) ExecuteWorkflow(context.Context, client.StartWorkflowOptions, interface{}, ...interface{}) (client.WorkflowRun, error) {
	c.starts.Add(1)
	return nil, errors.New("temporal unavailable")
}

// B: an epoch that fails during prepare (here ReattachAwaiters, e.g. Temporal
// visibility down) has already consumed the "one immediate takeover
// reconciliation". The same replica re-acquires a new epoch every
// RetryInterval, so a predecessor's dispatching row is re-reserved and retried
// every cycle although its durable start_retry_at lies in the future.
func TestReviewChurnedEpochsHonorStartBackoff(t *testing.T) {
	pool := controlTestPool(t)
	const instance = "backoff"
	predecessor, err := lease.New(lease.Config{Pool: pool, InstanceID: instance, Name: lease.ControllerName, Holder: "old"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := predecessor.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := predecessor.Release(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	// One fresh dispatching row owned by the released predecessor epoch.
	if _, err := pool.Exec(t.Context(), `INSERT INTO control.acceptance
 (instance_id,id,idem_key,actor,fingerprint,payload,state,owner_epoch,run_id,accepted_at,dispatch_started_at)
 VALUES ($1,'trigger-r1','r1','operator',decode('00','hex'),decode('78','hex'),'dispatching',$2,'r1',clock_timestamp(),clock_timestamp())`,
		instance, old.Epoch); err != nil {
		t.Fatal(err)
	}
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: instance, Name: lease.ControllerName, Holder: "replica"})
	if err != nil {
		t.Fatal(err)
	}
	var resolves atomic.Int32
	temporal := &countingTemporal{}
	attempts := make(chan struct{})
	runtime := Runtime{Configuration: testConfiguration{}, Pool: pool, InstanceID: instance, Lease: manager, Temporal: temporal, TaskQueue: "q",
		Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			resolves.Add(1)
			return engine.RunInput{AcceptanceID: r.ID, Gaggle: "g", WorkflowName: "w", SelfExecutionDenied: true}, nil
		},
		Services: func(context.Context, *lease.Handle) (EpochServices, error) {
			s := testServices()
			s.RestoreObservations = func(context.Context, []Observation) error { return nil }
			s.CaptureObservations = func(context.Context) ([]Observation, error) { return nil, nil }
			s.ReattachAwaiters = func(ctx context.Context) error {
				select {
				case attempts <- struct{}{}:
				case <-ctx.Done():
				}
				return errors.New("await reattachment: temporal visibility unavailable")
			}
			return s, nil
		}}
	host, err := New(Config{ValidateAssembly: func() error { return nil }, SelfExecution: "deny", Leadership: manager, SchemaCompatible: func(context.Context) error { return nil },
		BuildEpoch: runtime.Build, RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	for range 4 {
		<-attempts
	}
	cancel()
	<-done
	var count int
	var future bool
	if err := pool.QueryRow(t.Context(), `SELECT start_retry_count, start_retry_at > clock_timestamp() FROM control.acceptance WHERE id='trigger-r1'`).Scan(&count, &future); err != nil {
		t.Fatal(err)
	}
	t.Logf("4 failed epochs: resolver calls=%d temporal starts=%d start_retry_count=%d retry_at_in_future=%v", resolves.Load(), temporal.starts.Load(), count, future)
	if resolves.Load() > 1 {
		t.Fatalf("predecessor start retried %d times inside one backoff window (start_retry_count=%d)", resolves.Load(), count)
	}
}

// C: when epoch callbacks overrun StopTimeout, runEpoch returns, lease.Run
// voluntarily releases, and a successor can acquire while the old callback is
// still executing in this process (it can still issue unfenced effects such as
// Temporal starts or provider calls until the process exits).
func TestReviewStopTimeoutDoesNotHandOffWhileCallbackRuns(t *testing.T) {
	pool := controlTestPool(t)
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "overrun", Name: lease.ControllerName, Holder: "old"})
	if err != nil {
		t.Fatal(err)
	}
	entered, unblock := make(chan struct{}), make(chan struct{})
	var oldStillRunning atomic.Bool
	host, err := New(Config{ValidateAssembly: func() error { return nil }, SelfExecution: "deny", Leadership: manager, SchemaCompatible: func(context.Context) error { return nil },
		StopTimeout: 50 * time.Millisecond,
		BuildEpoch: func(context.Context, *lease.Handle) (EpochServices, error) {
			s := testServices()
			s.L1 = func(context.Context) error { // ignores cancellation, e.g. a hung provider RPC
				oldStillRunning.Store(true)
				close(entered)
				<-unblock
				oldStillRunning.Store(false)
				return nil
			}
			return s, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	<-entered
	cancel() // SIGTERM path
	runErr := <-done
	defer close(unblock)
	if !errors.Is(runErr, errStopTimeout) || !errors.Is(runErr, lease.ErrWorkStillRunning) {
		t.Fatalf("SIGTERM lost stop timeout: %v", runErr)
	}
	successor, err := lease.New(lease.Config{Pool: pool, InstanceID: "overrun", Name: lease.ControllerName, Holder: "new"})
	if err != nil {
		t.Fatal(err)
	}
	next, acquireErr := successor.Acquire(t.Context())
	if acquireErr == nil {
		defer func() { _ = successor.Release(context.Background(), next) }()
	}
	t.Logf("host.Run=%v; successor acquire err=%v; old callback still running=%v", runErr, acquireErr, oldStillRunning.Load())
	if !errors.Is(acquireErr, lease.ErrHeld) || !oldStillRunning.Load() {
		t.Fatalf("hung callback handed off early: acquire=%v running=%v", acquireErr, oldStillRunning.Load())
	}
}
