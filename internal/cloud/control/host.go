// Package control owns the cloud controller's lease-gated lifecycle. It never
// constructs a runner or executes stages; all workflow execution is remote.
package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// Leadership is the P3-03 lease lifecycle, including renewal and release.
type Leadership interface {
	Run(context.Context, func(context.Context, *lease.Handle) error) error
	SelfFenced(*lease.Handle) bool
}

// EpochServices must be reconstructed for every acquired epoch. Every mutable
// backend must bind this exact handle, rather than consulting a replaceable
// global owner from an old goroutine. Functions must honor their context.
type EpochServices struct {
	RebuildBeforeReap   func(context.Context) error
	ReconcileUncertain  func(context.Context) error
	ReattachAwaiters    func(context.Context) error
	L1                  func(context.Context) error
	L2                  func(context.Context) error
	L3                  func(context.Context) error
	Stop                func(context.Context) error
	RestoreObservations func(context.Context, []Observation) error
	CaptureObservations func(context.Context) ([]Observation, error)
}

func (e EpochServices) validate() error {
	for name, fn := range map[string]func(context.Context) error{
		"rebuild-before-reap": e.RebuildBeforeReap, "uncertain reconciliation": e.ReconcileUncertain,
		"await reattachment": e.ReattachAwaiters, "L1": e.L1, "L2": e.L2, "L3": e.L3, "stop": e.Stop,
	} {
		if fn == nil {
			return fmt.Errorf("cloud control requires %s wiring", name)
		}
	}
	return nil
}

// Config supplies process-level policy and epoch-scoped service construction.
type Config struct {
	ValidateAssembly func() error
	SelfExecution    string
	RunnerHosts      []string
	Leadership       Leadership
	SchemaCompatible func(context.Context) error
	BuildEpoch       func(context.Context, *lease.Handle) (EpochServices, error)
	Report           func(error)
	RetryInterval    time.Duration
	TickInterval     time.Duration
	StopTimeout      time.Duration
}

// ValidateBoot enforces D-2 independently of configuration admission.
func ValidateBoot(selfExecution string, runnerHosts []string) error {
	if selfExecution != "deny" {
		return errors.New("cloud control requires placement.selfExecution: deny")
	}
	for _, host := range runnerHosts {
		if host == "self" {
			return errors.New("cloud control runner inventory must not contain self")
		}
	}
	return nil
}

type epochState struct {
	ctx    context.Context
	handle *lease.Handle
	ready  bool
}

// Host runs one active controller with standby retry and truthful readiness.
type Host struct {
	config  Config
	state   atomic.Pointer[epochState]
	schema  atomic.Bool
	running atomic.Bool
}

// New validates the controller lifecycle without acquiring authority.
func New(config Config) (*Host, error) {
	if err := ValidateBoot(config.SelfExecution, config.RunnerHosts); err != nil {
		return nil, err
	}
	if config.ValidateAssembly == nil || config.Leadership == nil || config.SchemaCompatible == nil || config.BuildEpoch == nil {
		return nil, errors.New("cloud control requires assembly validation, leadership, schema check and epoch services")
	}
	if err := config.ValidateAssembly(); err != nil {
		return nil, fmt.Errorf("cloud control assembly: %w", err)
	}
	if config.RetryInterval == 0 {
		config.RetryInterval = 2 * time.Second
	}
	if config.TickInterval == 0 {
		config.TickInterval = time.Second
	}
	if config.StopTimeout == 0 {
		config.StopTimeout = 2 * time.Second
	}
	if config.RetryInterval <= 0 || config.TickInterval <= 0 || config.StopTimeout <= 0 {
		return nil, errors.New("cloud control intervals must be positive")
	}
	return &Host{config: config}, nil
}

// Ready requires compatible schema, completed recovery, and live authority.
func (h *Host) Ready() bool {
	state := h.state.Load()
	return h.schema.Load() && state != nil && state.ready && state.ctx.Err() == nil && !h.config.Leadership.SelfFenced(state.handle)
}

// Owner reports the live handle for lease-gated request planes, or nil.
func (h *Host) Owner() *lease.Handle {
	state := h.state.Load()
	if state == nil || !state.ready || !h.schema.Load() || state.ctx.Err() != nil || h.config.Leadership.SelfFenced(state.handle) {
		return nil
	}
	return state.handle
}

// Run remains a standby while another replica holds the lease. Every acquired
// epoch rebuilds before any reap/admission; cancellation releases voluntarily.
func (h *Host) Run(ctx context.Context) error {
	if !h.running.CompareAndSwap(false, true) {
		return errors.New("cloud control host is already running")
	}
	defer h.running.Store(false)
	defer h.state.Store(nil)
	backoff := h.config.RetryInterval
	for ctx.Err() == nil {
		err := h.checkSchema(ctx)
		if err == nil {
			err = h.config.Leadership.Run(ctx, h.runEpoch)
		}
		h.state.Store(nil)
		if errors.Is(err, errStopTimeout) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		delay := h.config.RetryInterval
		if err != nil && !errors.Is(err, lease.ErrHeld) {
			if h.config.Report != nil {
				h.config.Report(err)
			}
			delay = backoff
			backoff = min(backoff*2, max(time.Minute, h.config.RetryInterval))
		} else {
			backoff = h.config.RetryInterval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func (h *Host) checkSchema(ctx context.Context) error {
	check, cancel := context.WithTimeout(ctx, lease.FencedTransactionTimeout)
	defer cancel()
	err := h.config.SchemaCompatible(check)
	h.schema.Store(err == nil)
	return err
}

func (h *Host) runEpoch(ctx context.Context, handle *lease.Handle) error {
	epochCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.state.Store(&epochState{ctx: epochCtx, handle: handle})
	defer h.state.Store(nil)
	services, err := h.config.BuildEpoch(epochCtx, handle)
	if err == nil {
		err = services.validate()
	}
	if err != nil {
		cancel()
		if services.Stop != nil {
			return errors.Join(err, h.stop(services))
		}
		return err
	}
	if err := h.prepare(epochCtx, services); err != nil {
		cancel()
		return errors.Join(err, h.stop(services))
	}
	if epochCtx.Err() != nil {
		cancel()
		return errors.Join(epochCtx.Err(), h.stop(services))
	}
	h.state.Store(&epochState{ctx: epochCtx, handle: handle, ready: true})
	err = h.runLoops(epochCtx, cancel, services)
	cancel()
	return errors.Join(err, h.stop(services))
}

func (h *Host) prepare(ctx context.Context, services EpochServices) error {
	if err := services.RebuildBeforeReap(ctx); err != nil {
		return err
	}
	// Unknown starts remain quarantined; one slow Temporal observation cannot
	// indefinitely block attachment or healthy acceptance on this replica.
	reconcile, cancel := context.WithTimeout(ctx, lease.FencedTransactionTimeout)
	err := services.ReconcileUncertain(reconcile)
	cancel()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return services.ReattachAwaiters(ctx)
}

var errStopTimeout = fmt.Errorf("%w: cloud control callbacks did not stop within the shutdown bound", lease.ErrWorkStillRunning)

func (h *Host) stop(services EpochServices) error {
	ctx, cancel := context.WithTimeout(context.Background(), h.config.StopTimeout)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- services.Stop(ctx) }()
	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		return errStopTimeout
	}
}

func (h *Host) runLoops(ctx context.Context, cancel context.CancelFunc, services EpochServices) error {
	errs := make(chan error, 4)
	var workers sync.WaitGroup
	for _, tick := range []func(context.Context) error{services.L1, services.L2, services.L3, h.checkSchema} {
		workers.Go(func() { errs <- h.loop(ctx, tick) })
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errs:
	}
	cancel()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	timer := time.NewTimer(h.config.StopTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return err
	case <-timer.C:
		return errors.Join(err, errStopTimeout)
	}
}

func (h *Host) loop(ctx context.Context, tick func(context.Context) error) error {
	timer := time.NewTicker(h.config.TickInterval)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
