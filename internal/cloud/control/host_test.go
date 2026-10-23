package control

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
)

type fakeLeadership struct {
	fenced   atomic.Bool
	held     atomic.Bool
	released atomic.Bool
}

func (f *fakeLeadership) SelfFenced(*lease.Handle) bool { return f.fenced.Load() }
func (f *fakeLeadership) Run(ctx context.Context, work func(context.Context, *lease.Handle) error) error {
	if f.held.Load() {
		return lease.ErrHeld
	}
	defer f.released.Store(true)
	return work(ctx, &lease.Handle{InstanceID: "instance", Name: "control", Holder: "replica", Epoch: 1})
}
func testServices() EpochServices {
	ok := func(context.Context) error { return nil }
	return EpochServices{RebuildBeforeReap: ok, ReconcileUncertain: ok, ReattachAwaiters: ok, L1: ok, L2: ok, L3: ok, Stop: ok}
}
func testConfig(leader Leadership, build func(context.Context, *lease.Handle) (EpochServices, error)) Config {
	return Config{SelfExecution: "deny", Leadership: leader, SchemaCompatible: func(context.Context) error { return nil }, BuildEpoch: build, RetryInterval: time.Millisecond, TickInterval: 5 * time.Millisecond, StopTimeout: time.Second}
}
func TestRebuildBeforeReapOnAcquire(t *testing.T) {
	leader := &fakeLeadership{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	order := []string{}
	record := func(name string) func(context.Context) error {
		return func(context.Context) error { mu.Lock(); order = append(order, name); mu.Unlock(); return nil }
	}
	entered := make(chan struct{}, 1)
	var host *Host
	config := testConfig(leader, func(context.Context, *lease.Handle) (EpochServices, error) {
		services := testServices()
		services.RebuildBeforeReap = record("rebuild")
		services.ReconcileUncertain = record("reconcile")
		services.ReattachAwaiters = record("reattach")
		services.L2 = func(context.Context) error {
			if !host.Ready() {
				return errors.New("reap before ready")
			}
			select {
			case entered <- struct{}{}:
			default:
			}
			return nil
		}
		return services, nil
	})
	var err error
	host, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	if host.Ready() || host.Owner() != nil {
		t.Fatal("ready without lease")
	}
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("controller never became ready")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, []string{"rebuild", "reconcile", "reattach"}) {
		t.Fatalf("recovery order=%v", order)
	}
	if host.Ready() || host.Owner() != nil || !leader.released.Load() {
		t.Fatal("shutdown retained authority")
	}
}
func TestControlBootRefusesSelfExecution(t *testing.T) {
	for _, config := range []Config{{SelfExecution: "allow"}, {SelfExecution: ""}, {SelfExecution: "deny", RunnerHosts: []string{"self"}}} {
		if _, err := New(config); err == nil {
			t.Fatal("unsafe cloud boot allowed")
		}
	}
}
func TestReadinessImmediatelyRefusesSelfFenceAndSchemaLoss(t *testing.T) {
	leader := &fakeLeadership{}
	host, err := New(testConfig(leader, func(context.Context, *lease.Handle) (EpochServices, error) { return testServices(), nil }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	host.state.Store(&epochState{ctx: ctx, handle: &lease.Handle{}, ready: true})
	host.schema.Store(true)
	if !host.Ready() {
		t.Fatal("live initialized owner not ready")
	}
	leader.fenced.Store(true)
	if host.Ready() || host.Owner() != nil {
		t.Fatal("self-fenced owner stayed ready")
	}
	leader.fenced.Store(false)
	host.schema.Store(false)
	if host.Ready() || host.Owner() != nil {
		t.Fatal("incompatible schema stayed ready")
	}
	host.schema.Store(true)
	cancel()
	if host.Ready() || host.Owner() != nil {
		t.Fatal("cancelled owner stayed ready")
	}
}
func TestControlMissingRecoveryCannotBecomeReady(t *testing.T) {
	services := testServices()
	services.RebuildBeforeReap = nil
	if err := services.validate(); err == nil {
		t.Fatal("missing recovery silently treated as successful")
	}
}
func TestControlCancelsEpochBeforeStoppingAwaiters(t *testing.T) {
	leader := &fakeLeadership{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{}, 1)
	host, err := New(testConfig(leader, func(epoch context.Context, _ *lease.Handle) (EpochServices, error) {
		services := testServices()
		services.L1 = func(context.Context) error {
			select {
			case entered <- struct{}{}:
			default:
			}
			return nil
		}
		services.Stop = func(context.Context) error {
			if epoch.Err() == nil {
				return errors.New("awaiters stopped before epoch cancellation")
			}
			return nil
		}
		return services, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no active epoch")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
