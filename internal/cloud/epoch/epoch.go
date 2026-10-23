// Package epoch builds the control.EpochServices the single-active controller
// runs on every lease acquisition (HA-P3 group D, #7818).
//
// The order Host.prepare already enforces is: RebuildBeforeReap, then
// ReconcileUncertain, then ReattachAwaiters, then the loops. RebuildBeforeReap
// here is the correctness-critical step:
//
//  1. adopt, in one fenced statement per store, every row owned by an OLDER
//     epoch (cancel receipts, terminal reports, claims), so a predecessor that
//     crashed before its successor adopted never strands work;
//  2. list the open engine runs from Temporal (failure means liveness cannot be
//     proven);
//  3. renew the claims of runs that are open;
//  4. only then open the recovery gate, so the L2 reap is a no-op until the
//     renewal set has been rebuilt (reaping first could hand a live run's item
//     to a second run).
//
// A failure in any of those blocks serving: Host returns the error, never marks
// the epoch ready, releases the lease and retries. Per-run awaiter failures do
// not block readiness; they are reported and retried by the L2 loop.
//
// Every service is constructed per epoch and bound to that epoch's handle; a
// stale or self-fenced handle surfaces as lease.ErrStale / lease.ErrSelfFenced
// (IsStale), which ends the epoch.
package epoch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Adopter moves every older epoch's unfinished rows to the handle's epoch.
// cancelpg.Store and terminalreportpg.Store satisfy it.
type Adopter interface {
	AdoptAll(ctx context.Context, handle *lease.Handle) (int64, error)
}

// Claims is the claim plane's epoch maintenance (claims.Plane).
type Claims interface {
	Adopter
	RenewRuns(ctx context.Context, handle *lease.Handle, open map[string]bool) ([]localscheduler.ClaimEntry, error)
	// RecoverExpired releases an expired claim only when confirmClosed says its
	// run is CONFIRMED closed; an error or an open run keeps the claim.
	RecoverExpired(ctx context.Context, handle *lease.Handle, gate interface{ RecoveryPermitted() bool }, confirmClosed func(ctx context.Context, runID string) (closed bool, err error)) ([]localscheduler.ClaimEntry, error)
}

// OpenRuns lists the workflow IDs of runs that are open in Temporal. The listing
// comes from visibility, which is eventually consistent (it lags exactly at a
// failover) and excludes paused runs, so it is only a RENEWAL hint: it is never
// treated as proof that an absent run is dead. Reaping is gated on ConfirmClosed.
type OpenRuns interface {
	OpenRuns(ctx context.Context) (map[string]bool, error)
}

// Awaiters owns the leader-side awaiter per open engine run and the terminal
// effects it emits. Reattach starts awaiters for open runs; per-run failures
// are reported through Config.Report and retried by Tick, not returned.
type Awaiters interface {
	Reattach(ctx context.Context, handle *lease.Handle, open map[string]bool) error
	Tick(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Observations snapshots and restores provider observations across epochs.
type Observations struct {
	Restore func(context.Context, []control.Observation) error
	Capture func(context.Context) ([]control.Observation, error)
}

// Config supplies the stores and hooks of one epoch.
type Config struct {
	Handle   *lease.Handle
	Claims   Claims
	Cancel   Adopter
	Terminal Adopter
	Open     OpenRuns
	// ConfirmClosed answers, from a strongly consistent source (Temporal
	// DescribeWorkflowExecution), whether a run is CLOSED: not found or a closed
	// status is closed; running or paused is open; an error is neither, and the
	// claim is kept this tick.
	ConfirmClosed func(ctx context.Context, runID string) (closed bool, err error)
	// Awaiters may be nil until the terminal-effect awaiters are composed.
	Awaiters     Awaiters
	Observations Observations
	// ReconcileUncertain, L1, L2Extra and L3 are optional epoch hooks (the
	// scheduler tick, GC, config apply); nil hooks are no-ops.
	ReconcileUncertain func(context.Context) error
	L1                 func(context.Context) error
	L2Extra            func(context.Context) error
	L3                 func(context.Context) error
	Report             func(error)
	// RenewEvery throttles the L2 liveness listing, renewal and reap (default
	// 15s); Now is the clock for that throttle (default time.Now).
	RenewEvery time.Duration
	Now        func() time.Time
}

func (c Config) validate() error {
	switch {
	case c.Handle == nil:
		return errors.New("cloud epoch requires the acquired lease handle")
	case c.Claims == nil || c.Cancel == nil || c.Terminal == nil:
		return errors.New("cloud epoch requires claims, cancel-receipt and terminal-report adoption")
	case c.Open == nil:
		return errors.New("cloud epoch requires an open-run lister")
	case c.ConfirmClosed == nil:
		return errors.New("cloud epoch requires a strongly consistent run-closed confirmation")
	case c.Observations.Restore == nil || c.Observations.Capture == nil:
		return errors.New("cloud epoch requires provider observation wiring")
	}
	return nil
}

// IsStale reports whether err means this epoch's authority is gone (stale or
// self-fenced handle) and the epoch must end rather than retry in place.
func IsStale(err error) bool {
	return errors.Is(err, lease.ErrStale) || errors.Is(err, lease.ErrSelfFenced)
}

// Build returns the epoch services for cfg.Handle.
func Build(cfg Config) (control.EpochServices, error) {
	if err := cfg.validate(); err != nil {
		return control.EpochServices{}, err
	}
	e := &services{cfg: cfg, gate: localscheduler.NewRecoveryGate()}
	return control.EpochServices{
		RebuildBeforeReap:   e.rebuild,
		ReconcileUncertain:  optional(cfg.ReconcileUncertain),
		ReattachAwaiters:    e.reattach,
		L1:                  optional(cfg.L1),
		L2:                  e.l2,
		L3:                  optional(cfg.L3),
		Stop:                e.stop,
		RestoreObservations: cfg.Observations.Restore,
		CaptureObservations: cfg.Observations.Capture,
	}, nil
}

func optional(fn func(context.Context) error) func(context.Context) error {
	if fn == nil {
		return func(context.Context) error { return nil }
	}
	return fn
}

type services struct {
	cfg  Config
	gate *localscheduler.RecoveryGate

	mu     sync.Mutex
	open   map[string]bool
	lastL2 time.Time
}

const defaultRenewEvery = 15 * time.Second

func (e *services) now() time.Time {
	if e.cfg.Now != nil {
		return e.cfg.Now()
	}
	return time.Now()
}

// due reports whether the throttled L2 maintenance should run now, recording
// the attempt either way so a failing listing is not hammered every tick.
func (e *services) due() bool {
	every := e.cfg.RenewEvery
	if every <= 0 {
		every = defaultRenewEvery
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if !e.lastL2.IsZero() && now.Sub(e.lastL2) < every {
		return false
	}
	e.lastL2 = now
	return true
}

// rebuild adopts, proves liveness, renews, and only then opens the reap gate.
func (e *services) rebuild(ctx context.Context) error {
	h := e.cfg.Handle
	for _, step := range []struct {
		name    string
		adopter Adopter
	}{{"cancel receipts", e.cfg.Cancel}, {"terminal reports", e.cfg.Terminal}, {"claims", e.cfg.Claims}} {
		if _, err := step.adopter.AdoptAll(ctx, h); err != nil {
			return fmt.Errorf("cloud epoch %d: adopt %s: %w", h.Epoch, step.name, err)
		}
	}
	open, err := e.cfg.Open.OpenRuns(ctx)
	if err != nil {
		return fmt.Errorf("cloud epoch %d: list open runs: %w", h.Epoch, err)
	}
	if _, err := e.cfg.Claims.RenewRuns(ctx, h, open); err != nil {
		return fmt.Errorf("cloud epoch %d: renew claims of open runs: %w", h.Epoch, err)
	}
	e.mu.Lock()
	e.open = open
	e.mu.Unlock()
	e.gate.MarkRenewalRebuilt()
	return nil
}

func (e *services) reattach(ctx context.Context) error {
	if e.cfg.Awaiters == nil {
		return nil
	}
	e.mu.Lock()
	open := e.open
	e.mu.Unlock()
	return e.cfg.Awaiters.Reattach(ctx, e.cfg.Handle, open)
}

// l2 renews, reaps (gated), then runs the awaiter tick and any extra hook.
// Per-step failures are joined so one failing step does not starve the rest.
// l2 runs the throttled maintenance. A non-stale failure is reported and the
// loop continues (Host.loop would otherwise tear the whole epoch down on any
// tick error); only a fenced-out handle ends the epoch.
func (e *services) l2(ctx context.Context) error {
	err := e.maintain(ctx)
	if err == nil || IsStale(err) {
		return err
	}
	if e.cfg.Report != nil {
		e.cfg.Report(err)
	}
	return nil
}

func (e *services) maintain(ctx context.Context) error {
	if e.cfg.Awaiters != nil {
		// The awaiter tick is cheap and not throttled.
		defer func() {
			if err := e.cfg.Awaiters.Tick(ctx); err != nil && e.cfg.Report != nil {
				e.cfg.Report(err)
			}
		}()
	}
	if !e.due() {
		return e.extra(ctx)
	}
	h := e.cfg.Handle
	open, err := e.cfg.Open.OpenRuns(ctx)
	if err != nil {
		// Without a liveness proof neither renew nor reap is safe this tick.
		return fmt.Errorf("cloud epoch %d: list open runs: %w", h.Epoch, err)
	}
	var errs []error
	if _, err := e.cfg.Claims.RenewRuns(ctx, h, open); err != nil {
		// A failed renewal also skips the reap: an expired claim of an open run
		// must not be released because its renewal was lost.
		return fmt.Errorf("cloud epoch %d: renew claims: %w", h.Epoch, err)
	}
	if _, err := e.cfg.Claims.RecoverExpired(ctx, h, e.gate, e.cfg.ConfirmClosed); err != nil {
		errs = append(errs, fmt.Errorf("cloud epoch %d: recover expired claims: %w", h.Epoch, err))
	}
	if err := e.extra(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (e *services) extra(ctx context.Context) error {
	if e.cfg.L2Extra == nil {
		return nil
	}
	return e.cfg.L2Extra(ctx)
}

func (e *services) stop(ctx context.Context) error {
	if e.cfg.Awaiters == nil {
		return nil
	}
	return e.cfg.Awaiters.Stop(ctx)
}
