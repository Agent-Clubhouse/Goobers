package epoch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/localscheduler"
)

type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

type adopter struct {
	name string
	rec  *recorder
	err  error
}

func (a adopter) AdoptAll(_ context.Context, h *lease.Handle) (int64, error) {
	a.rec.add(fmt.Sprintf("adopt:%s@%d", a.name, h.Epoch))
	return 1, a.err
}

type fakeClaims struct {
	adopter
	renewErr, recoverErr error
	renewed              map[string]bool
	reaped               int
}

func (c *fakeClaims) RenewRuns(_ context.Context, _ *lease.Handle, open map[string]bool) ([]localscheduler.ClaimEntry, error) {
	c.rec.add("renew")
	c.renewed = open
	return nil, c.renewErr
}

// RecoverExpired honors the gate exactly as the claim plane does: a closed gate
// is a no-op.
func (c *fakeClaims) RecoverExpired(_ context.Context, _ *lease.Handle, gate interface{ RecoveryPermitted() bool }) ([]localscheduler.ClaimEntry, error) {
	if gate == nil || !gate.RecoveryPermitted() {
		c.rec.add("reap:gated")
		return nil, nil
	}
	c.rec.add("reap")
	c.reaped++
	return nil, c.recoverErr
}

type fakeOpen struct {
	runs map[string]bool
	err  error
	rec  *recorder
}

func (o fakeOpen) OpenRuns(context.Context) (map[string]bool, error) {
	o.rec.add("open")
	return o.runs, o.err
}

type fakeAwaiters struct {
	rec        *recorder
	reattached map[string]bool
	tickErr    error
}

func (a *fakeAwaiters) Reattach(_ context.Context, _ *lease.Handle, open map[string]bool) error {
	a.rec.add("reattach")
	a.reattached = open
	return nil
}
func (a *fakeAwaiters) Tick(context.Context) error { a.rec.add("tick"); return a.tickErr }
func (a *fakeAwaiters) Stop(context.Context) error { a.rec.add("stop"); return nil }

func newConfig(rec *recorder) (Config, *fakeClaims, *fakeAwaiters) {
	claims := &fakeClaims{adopter: adopter{name: "claims", rec: rec}}
	awaiters := &fakeAwaiters{rec: rec}
	return Config{
		Handle: &lease.Handle{InstanceID: "i", Epoch: 3},
		Claims: claims, Cancel: adopter{name: "cancel", rec: rec}, Terminal: adopter{name: "terminal", rec: rec},
		Open:     fakeOpen{runs: map[string]bool{"run-1": true}, rec: rec},
		Awaiters: awaiters,
		Observations: Observations{
			Restore: func(context.Context, []control.Observation) error { return nil },
			Capture: func(context.Context) ([]control.Observation, error) { return nil, nil },
		},
	}, claims, awaiters
}

func TestRebuildAdoptsEveryOlderEpochBeforeRenewingAndOpensTheGateLast(t *testing.T) {
	rec := &recorder{}
	cfg, claims, awaiters := newConfig(rec)
	services, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := services.RebuildBeforeReap(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"adopt:cancel@3", "adopt:terminal@3", "adopt:claims@3", "open", "renew"}
	if got := rec.list(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rebuild order = %v, want %v", got, want)
	}
	if !claims.renewed["run-1"] {
		t.Fatal("renewal did not receive the open-run set")
	}
	if err := services.ReattachAwaiters(t.Context()); err != nil || !awaiters.reattached["run-1"] {
		t.Fatalf("reattach = %v with %v; want the open set from the rebuild", err, awaiters.reattached)
	}
}

func TestReapIsAGatedNoOpUntilTheRenewalSetIsRebuilt(t *testing.T) {
	rec := &recorder{}
	cfg, claims, _ := newConfig(rec)
	now := time.Unix(0, 0)
	cfg.Now = func() time.Time { return now }
	services, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := services.L2(t.Context()); err != nil {
		t.Fatal(err)
	}
	if claims.reaped != 0 {
		t.Fatal("L2 reaped before the renewal set was rebuilt")
	}
	now = now.Add(time.Minute)
	if err := services.RebuildBeforeReap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := services.L2(t.Context()); err != nil || claims.reaped != 1 {
		t.Fatalf("after rebuild: reaped %d, err %v; want 1", claims.reaped, err)
	}
}

func TestRebuildFailsClosedAndNeverOpensTheGate(t *testing.T) {
	boom := errors.New("boom")
	for name, mutate := range map[string]func(*Config, *fakeClaims){
		"cancel adoption":   func(c *Config, _ *fakeClaims) { c.Cancel = adopter{name: "cancel", rec: &recorder{}, err: boom} },
		"terminal adoption": func(c *Config, _ *fakeClaims) { c.Terminal = adopter{name: "terminal", rec: &recorder{}, err: boom} },
		"claims adoption":   func(_ *Config, cl *fakeClaims) { cl.err = boom },
		"open-run listing":  func(c *Config, _ *fakeClaims) { c.Open = fakeOpen{err: boom, rec: &recorder{}} },
		"claim renewal":     func(_ *Config, cl *fakeClaims) { cl.renewErr = boom },
	} {
		rec := &recorder{}
		cfg, claims, _ := newConfig(rec)
		mutate(&cfg, claims)
		now := time.Unix(0, 0)
		cfg.Now = func() time.Time { return now }
		services, err := Build(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := services.RebuildBeforeReap(t.Context()); !errors.Is(err, boom) {
			t.Errorf("%s: rebuild = %v, want the failure", name, err)
		}
		now = now.Add(time.Hour)
		_ = services.L2(t.Context())
		if claims.reaped != 0 {
			t.Errorf("%s: a failed rebuild still allowed a reap", name)
		}
	}
}

// switchableOpen succeeds until told to fail, like a visibility store going down
// mid-epoch.
type switchableOpen struct {
	mu   sync.Mutex
	fail bool
	rec  *recorder
}

func (o *switchableOpen) OpenRuns(context.Context) (map[string]bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rec.add("open")
	if o.fail {
		return nil, errors.New("visibility down")
	}
	return map[string]bool{"run-1": true}, nil
}

func TestL2SkipsRenewAndReapWithoutALivenessProof(t *testing.T) {
	rec := &recorder{}
	cfg, claims, _ := newConfig(rec)
	open := &switchableOpen{rec: rec}
	cfg.Open = open
	now := time.Unix(0, 0)
	cfg.Now = func() time.Time { return now }
	services, _ := Build(cfg)
	if err := services.RebuildBeforeReap(t.Context()); err != nil {
		t.Fatal(err)
	}
	open.fail = true
	now = now.Add(time.Hour)
	renewsBefore := 0
	for _, call := range rec.list() {
		if call == "renew" {
			renewsBefore++
		}
	}
	if err := services.L2(t.Context()); err == nil {
		t.Fatal("L2 succeeded without a liveness proof")
	}
	renews := 0
	for _, call := range rec.list() {
		if call == "renew" {
			renews++
		}
	}
	if renews != renewsBefore || claims.reaped != 0 {
		t.Fatalf("renews %d (was %d) reaped %d; neither may run without a liveness proof", renews, renewsBefore, claims.reaped)
	}
}

func TestL2ThrottlesMaintenance(t *testing.T) {
	rec := &recorder{}
	cfg, claims, _ := newConfig(rec)
	now := time.Unix(0, 0)
	cfg.Now = func() time.Time { return now }
	cfg.RenewEvery = time.Minute
	services, _ := Build(cfg)
	if err := services.RebuildBeforeReap(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := services.L2(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if claims.reaped != 1 {
		t.Fatalf("reaped %d times within one RenewEvery window, want 1", claims.reaped)
	}
	now = now.Add(2 * time.Minute)
	if err := services.L2(t.Context()); err != nil || claims.reaped != 2 {
		t.Fatalf("after the window: reaped %d, err %v; want 2", claims.reaped, err)
	}
}

func TestAwaiterTickFailureDoesNotFailL2OrBlockReap(t *testing.T) {
	rec := &recorder{}
	cfg, claims, awaiters := newConfig(rec)
	awaiters.tickErr = errors.New("one run's hook failed")
	var reported []error
	cfg.Report = func(err error) { reported = append(reported, err) }
	cfg.Now = func() time.Time { return time.Unix(0, 0) }
	services, _ := Build(cfg)
	if err := services.RebuildBeforeReap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := services.L2(t.Context()); err != nil {
		t.Fatalf("L2 = %v; a per-run awaiter failure must not fail the loop", err)
	}
	if claims.reaped != 1 || len(reported) != 1 {
		t.Fatalf("reaped %d reported %v; want the reap to run and the failure reported", claims.reaped, reported)
	}
}

func TestStopStopsAwaiters(t *testing.T) {
	rec := &recorder{}
	cfg, _, _ := newConfig(rec)
	services, _ := Build(cfg)
	if err := services.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := rec.list(); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("calls = %v, want the awaiters stopped", got)
	}
}

func TestBuildRefusesIncompleteConfig(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no handle":      func(c *Config) { c.Handle = nil },
		"no claims":      func(c *Config) { c.Claims = nil },
		"no cancel":      func(c *Config) { c.Cancel = nil },
		"no terminal":    func(c *Config) { c.Terminal = nil },
		"no open lister": func(c *Config) { c.Open = nil },
		"no restore":     func(c *Config) { c.Observations.Restore = nil },
		"no capture":     func(c *Config) { c.Observations.Capture = nil },
	} {
		cfg, _, _ := newConfig(&recorder{})
		mutate(&cfg)
		if _, err := Build(cfg); err == nil {
			t.Errorf("%s: built", name)
		}
	}
}

func TestIsStale(t *testing.T) {
	for _, err := range []error{lease.ErrStale, fmt.Errorf("wrapped: %w", lease.ErrSelfFenced)} {
		if !IsStale(err) {
			t.Errorf("%v not stale", err)
		}
	}
	if IsStale(errors.New("other")) || IsStale(nil) {
		t.Error("an unrelated error read as stale")
	}
}

type leadership struct{ handle *lease.Handle }

func (l leadership) Run(ctx context.Context, fn func(context.Context, *lease.Handle) error) error {
	return fn(ctx, l.handle)
}
func (leadership) SelfFenced(*lease.Handle) bool { return false }

func newHost(t *testing.T, cfg Config, report func(error)) *control.Host {
	t.Helper()
	host, err := control.New(control.Config{
		ValidateAssembly: func() error { return nil }, SelfExecution: "deny",
		Leadership: leadership{handle: cfg.Handle}, SchemaCompatible: func(context.Context) error { return nil },
		BuildEpoch: func(context.Context, *lease.Handle) (control.EpochServices, error) { return Build(cfg) },
		Report:     report, RetryInterval: time.Hour, TickInterval: time.Millisecond, StopTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

// TestHostIsReadyOnlyAfterTheRebuildSucceeds drives the real Host: a healthy
// rebuild reaches the loops (observed from L1, which runs only once ready); a
// failing adoption surfaces through Report and the host is never ready.
func TestHostIsReadyOnlyAfterTheRebuildSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg, _, _ := newConfig(&recorder{})
	var host *control.Host
	ready := make(chan bool, 1)
	cfg.L1 = func(context.Context) error {
		select {
		case ready <- host.Ready():
		default:
		}
		return nil
	}
	host = newHost(t, cfg, func(error) {})
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	if !<-ready {
		t.Fatal("L1 ran before the host was ready")
	}
	cancel()
	<-done

	failing, _, _ := newConfig(&recorder{})
	failing.Terminal = adopter{name: "terminal", rec: &recorder{}, err: errors.New("terminal adoption failed")}
	var l1Ran bool
	failing.L1 = func(context.Context) error { l1Ran = true; return nil }
	reported := make(chan error, 1)
	failingHost := newHost(t, failing, func(err error) {
		select {
		case reported <- err:
		default:
		}
	})
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	done2 := make(chan error, 1)
	go func() { done2 <- failingHost.Run(ctx2) }()
	if err := <-reported; err == nil {
		t.Fatal("no error reported")
	}
	if failingHost.Ready() || l1Ran {
		t.Fatalf("host served (ready=%v, L1 ran=%v) after a failed adoption", failingHost.Ready(), l1Ran)
	}
	cancel2()
	<-done2
}
