// Package terminalawait is the cloud controller's leader-owned awaiter: it waits
// for each engine run to close in Temporal and fires the terminal-hook frame
// (internal/terminalhooks/frame) with the run's outcome. It is the cloud
// counterpart of the deleted daemon awaitInto (#7783).
//
// An awaiter is bound to one lease epoch's handle and stops with it. Within an
// epoch the frame fires once per run: a completed run is remembered, and only a
// failed Finalize (the claim release) is retried, with capped backoff. ACROSS
// epochs a successor may re-fire what a crashed leader began, so every hook must
// be idempotent. The frame already requires it of Finalize; a hook with an
// external side effect (3b: parking labels, comments, branch delete) must carry
// a durable idempotency key derived from the run, either an IdempotencyKey on
// the provider write or an effect key in the terminal_report outbox.
//
// A run is picked up while it is open, while it still holds claims, and, so that
// a run which closed while no leader was awaiting it (a leader gap, the
// concurrency cap) is never lost, by a durable closed-run watermark: every Tick
// lists the runs closed after the watermark (less a skew margin), and the
// watermark advances only past runs whose frame completed. Closed runs therefore
// fire late, never not at all. A failed attempt is reported and retried on the
// next Tick; it never blocks readiness.
package terminalawait

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/terminalhooks/frame"
)

// Workflows is the Temporal surface the awaiter needs; client.Client satisfies it.
type Workflows interface {
	GetWorkflow(ctx context.Context, workflowID, runID string) client.WorkflowRun
}

// Identity names a run for the frame.
type Identity struct {
	Gaggle   string
	Workflow string
	RepoRef  apiv1.RepoRef
	// Item is the driving backlog item when the run was started with one; nil
	// for a run that claims its item mid-walk (the hooks resolve it from claims).
	Item     *apiv1.BacklogItem
	Backprop bool
}

// Config supplies one epoch's awaiter.
type Config struct {
	Temporal Workflows
	// Identify resolves a run's identity (from its acceptance and pinned
	// generation). An error is reported and retried.
	Identify func(ctx context.Context, runID string) (Identity, error)
	// Open lists the runs open in Temporal (a discovery hint, like the epoch's).
	Open func(ctx context.Context) (map[string]bool, error)
	// ClaimHolders lists runs that currently hold claims, so a run that closed
	// before its hooks fired is still picked up. Optional.
	ClaimHolders func(ctx context.Context) ([]string, error)
	// Closed lists the runs that closed after since, with their close times (a
	// discovery hint: visibility is eventually consistent, hence Margin).
	// Optional; with Watermark it makes discovery of closed runs durable.
	Closed func(ctx context.Context, since time.Time) ([]ClosedRun, error)
	// Watermark persists how far closed-run discovery has completed. Required
	// with Closed.
	Watermark Watermark
	// Margin is how far before the watermark Closed looks, to cover clock skew
	// between hosts (default 10m). The first epoch ever initialises the watermark
	// from this host's clock, so skew must stay under Margin.
	Margin time.Duration
	// LagBound is the maximum Temporal visibility lag tolerated (default 1h): the
	// watermark never advances past now-LagBound, so a run whose visibility
	// record appears later than that after it closed is the one case that can be
	// missed. Larger values widen each discovery pass.
	LagBound time.Duration
	// ClosedEvery throttles closed-run discovery (default 1m): each pass pages
	// every closure in the LagBound window, far too heavy for every L2 tick.
	ClosedEvery time.Duration
	// RetryBase and RetryMax bound the backoff of a failed Finalize (defaults 2s
	// and 5m).
	RetryBase, RetryMax time.Duration
	Now                 func() time.Time
	// Hooks builds the frame for one run's identity.
	Hooks func(handle *lease.Handle, id Identity) *frame.Hooks
	// MaxConcurrent bounds simultaneous awaiters (default 256); further runs
	// wait for the next Tick.
	MaxConcurrent int
	Report        func(error)
}

// ClosedRun is a run observed closed in Temporal visibility.
type ClosedRun struct {
	RunID     string
	CloseTime time.Time
}

// Watermark is the durable closed-run discovery position (terminalwatermarkpg).
type Watermark interface {
	// Load returns the position, false when none was ever recorded.
	Load(ctx context.Context) (time.Time, bool, error)
	// Advance moves it forward under the epoch's fence.
	Advance(ctx context.Context, handle *lease.Handle, through time.Time) error
}

// retryState is a run whose frame ran but whose Finalize failed.
type retryState struct {
	hooks    *frame.Hooks
	out      frame.Outcome
	attempts int
	next     time.Time
}

// Awaiters is the epoch's awaiter set (it satisfies epoch.Awaiters).
type Awaiters struct {
	cfg    Config
	handle *lease.Handle

	mu      sync.Mutex
	running map[string]struct{}
	// done holds the runs whose frame completed (or that this instance does not
	// own) in this epoch; they are never fired again here.
	done  map[string]struct{}
	retry map[string]*retryState
	// closedAt is the close time of each closed run discovered but not yet done;
	// the watermark cannot pass the earliest of them.
	closedAt    map[string]time.Time
	watermark   time.Time
	initialized bool
	lastClosed  time.Time
	cancel      context.CancelFunc
	ctx         context.Context
	stopping    bool
	wg          sync.WaitGroup
}

// New returns the awaiters for one acquired epoch.
func New(handle *lease.Handle, cfg Config) (*Awaiters, error) {
	switch {
	case handle == nil:
		return nil, errors.New("terminal awaiter requires the acquired lease handle")
	case cfg.Temporal == nil || cfg.Identify == nil || cfg.Open == nil || cfg.Hooks == nil:
		return nil, errors.New("terminal awaiter requires Temporal, run identification, open-run discovery and hooks")
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 256
	}
	switch {
	case cfg.Closed != nil && cfg.Watermark == nil:
		return nil, errors.New("terminal awaiter: closed-run discovery requires a durable watermark")
	}
	if cfg.Margin <= 0 {
		cfg.Margin = 10 * time.Minute
	}
	if cfg.LagBound <= 0 {
		cfg.LagBound = time.Hour
	}
	if cfg.ClosedEvery <= 0 {
		cfg.ClosedEvery = time.Minute
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 2 * time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Awaiters{
		cfg: cfg, handle: handle,
		running: map[string]struct{}{}, done: map[string]struct{}{},
		retry: map[string]*retryState{}, closedAt: map[string]time.Time{},
	}, nil
}

// Reattach starts an awaiter for every open run and every closed run still
// holding claims. Enumeration failure is returned (nothing can be attached);
// per-run failures are reported and retried by Tick.
func (a *Awaiters) Reattach(ctx context.Context, handle *lease.Handle, open map[string]bool) error {
	if handle != a.handle {
		return errors.New("terminal awaiter: reattach for a different epoch's handle")
	}
	a.mu.Lock()
	if a.ctx == nil {
		// The awaiters outlive the Reattach call (its ctx is the prepare ctx):
		// they run until Stop, bound to the epoch via Stop.
		a.ctx, a.cancel = context.WithCancel(context.WithoutCancel(ctx))
	}
	a.mu.Unlock()
	return a.discover(ctx, open)
}

// Tick discovers runs started or closed since the last pass and retries the
// attempts that failed.
func (a *Awaiters) Tick(ctx context.Context) error {
	open, err := a.cfg.Open(ctx)
	if err != nil {
		return fmt.Errorf("terminal awaiter: list open runs: %w", err)
	}
	return a.discover(ctx, open)
}

func (a *Awaiters) discover(ctx context.Context, open map[string]bool) error {
	runs := make([]string, 0, len(open))
	for run := range open {
		runs = append(runs, run)
	}
	if a.cfg.ClaimHolders != nil {
		holders, err := a.cfg.ClaimHolders(ctx)
		if err != nil {
			return fmt.Errorf("terminal awaiter: list claim holders: %w", err)
		}
		runs = append(runs, holders...)
	}
	closed, err := a.discoverClosed(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		a.start(run)
	}
	for _, run := range closed {
		a.start(run.RunID)
	}
	a.startRetries()
	a.advanceWatermark(ctx)
	return nil
}

// discoverClosed lists the runs closed since the durable watermark (less the
// margin), earliest first. The first epoch ever starts the watermark at now.
func (a *Awaiters) discoverClosed(ctx context.Context) ([]ClosedRun, error) {
	if a.cfg.Closed == nil {
		return nil, nil
	}
	a.mu.Lock()
	initialized, mark := a.initialized, a.watermark
	if initialized && a.cfg.Now().Sub(a.lastClosed) < a.cfg.ClosedEvery {
		a.mu.Unlock()
		return nil, nil // throttled: the pending closed runs are already being framed
	}
	a.mu.Unlock()
	if !initialized {
		through, ok, err := a.cfg.Watermark.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("terminal awaiter: load watermark: %w", err)
		}
		if !ok {
			through = a.cfg.Now()
			if err := a.cfg.Watermark.Advance(ctx, a.handle, through); err != nil {
				return nil, fmt.Errorf("terminal awaiter: initialise watermark: %w", err)
			}
		}
		a.mu.Lock()
		a.initialized, a.watermark, mark = true, through, through
		a.mu.Unlock()
	}
	closed, err := a.cfg.Closed(ctx, mark.Add(-a.cfg.Margin))
	if err != nil {
		return nil, fmt.Errorf("terminal awaiter: list closed runs: %w", err)
	}
	sort.Slice(closed, func(i, j int) bool { return closed[i].CloseTime.Before(closed[j].CloseTime) })
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastClosed = a.cfg.Now()
	fresh := closed[:0:0]
	for _, run := range closed {
		if _, ok := a.done[run.RunID]; ok {
			continue
		}
		a.closedAt[run.RunID] = run.CloseTime
		fresh = append(fresh, run)
	}
	return fresh, nil
}

// advanceWatermark moves the durable position to now-LagBound, or to just before
// the earliest closed run still unframed if that is earlier. Never past
// now-LagBound: a run's visibility record may lag its close by up to LagBound,
// and later closures must not carry the watermark over a run not yet listed. A
// failure is reported and leaves the position where it was (late, never lost).
func (a *Awaiters) advanceWatermark(ctx context.Context) {
	if a.cfg.Closed == nil {
		return
	}
	a.mu.Lock()
	target := a.cfg.Now().Add(-a.cfg.LagBound)
	for _, at := range a.closedAt {
		if held := at.Add(-time.Nanosecond); held.Before(target) {
			target = held
		}
	}
	if !target.After(a.watermark) {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	if err := a.cfg.Watermark.Advance(ctx, a.handle, target); err != nil {
		a.report(ctx, fmt.Errorf("terminal awaiter: advance watermark: %w", err))
		return
	}
	a.mu.Lock()
	if target.After(a.watermark) {
		a.watermark = target
	}
	a.mu.Unlock()
}

// startRetries re-runs Finalize for every run whose backoff has elapsed.
func (a *Awaiters) startRetries() {
	now := a.cfg.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping || a.ctx == nil {
		return
	}
	for runID, st := range a.retry {
		if _, ok := a.running[runID]; ok || len(a.running) >= a.cfg.MaxConcurrent || now.Before(st.next) {
			continue
		}
		a.running[runID] = struct{}{}
		a.wg.Add(1)
		go a.refinalize(a.ctx, runID, st)
	}
}

func (a *Awaiters) start(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping || a.ctx == nil {
		return
	}
	if _, ok := a.running[runID]; ok || len(a.running) >= a.cfg.MaxConcurrent {
		return
	}
	if _, ok := a.done[runID]; ok {
		return
	}
	if _, ok := a.retry[runID]; ok {
		return // only its Finalize is outstanding; startRetries owns it
	}
	a.running[runID] = struct{}{}
	a.wg.Add(1)
	go a.await(a.ctx, runID)
}

func (a *Awaiters) await(ctx context.Context, runID string) {
	defer a.wg.Done()
	defer func() {
		a.mu.Lock()
		delete(a.running, runID)
		a.mu.Unlock()
	}()
	id, err := a.cfg.Identify(ctx, runID)
	if errors.Is(err, ErrUnidentifiable) {
		// A generated child, or a run this instance never accepted: its owner
		// handles it. Remembered, so it is neither re-identified nor reported.
		a.markDone(runID)
		return
	}
	if err != nil {
		a.report(ctx, fmt.Errorf("terminal awaiter: identify run %s: %w", runID, err))
		return
	}
	var result engine.RunResult
	runErr := a.cfg.Temporal.GetWorkflow(ctx, runID, "").Get(ctx, &result)
	if ctx.Err() != nil {
		return // the epoch is ending: this is not the run's terminal
	}
	out := frame.Outcome{
		RunID: runID, Gaggle: id.Gaggle, Workflow: id.Workflow, Backprop: id.Backprop,
		Result: result, Item: id.Item, Err: runErr,
	}
	if runErr != nil {
		out.Result = engine.RunResult{} // a walk-level failure carries no status
	}
	out.Phase = frame.PhaseFor(out.Result)
	hooks := a.cfg.Hooks(a.handle, id)
	if err := hooks.RunFinalizing(ctx, out); err != nil {
		if ctx.Err() == nil {
			a.scheduleRetry(runID, &retryState{hooks: hooks, out: out, attempts: 1}, ctx)
		}
		return
	}
	a.markDone(runID)
}

// refinalize retries only the claim release of a run whose frame already ran.
func (a *Awaiters) refinalize(ctx context.Context, runID string, st *retryState) {
	defer a.wg.Done()
	defer func() {
		a.mu.Lock()
		delete(a.running, runID)
		a.mu.Unlock()
	}()
	if err := st.hooks.Refinalize(ctx, st.out); err != nil {
		if ctx.Err() == nil {
			st.attempts++
			a.scheduleRetry(runID, st, ctx)
		}
		return
	}
	a.mu.Lock()
	delete(a.retry, runID)
	a.mu.Unlock()
	a.markDone(runID)
}

func (a *Awaiters) scheduleRetry(runID string, st *retryState, ctx context.Context) {
	backoff := a.cfg.RetryBase
	for i := 1; i < st.attempts && backoff < a.cfg.RetryMax; i++ {
		backoff *= 2
	}
	if backoff > a.cfg.RetryMax {
		backoff = a.cfg.RetryMax
	}
	st.next = a.cfg.Now().Add(backoff)
	a.mu.Lock()
	a.retry[runID] = st
	a.mu.Unlock()
	a.report(ctx, fmt.Errorf("terminal awaiter: finalize run %s failed (attempt %d), retrying after %s", runID, st.attempts, backoff))
}

func (a *Awaiters) markDone(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done[runID] = struct{}{}
	delete(a.closedAt, runID)
}

func (a *Awaiters) report(ctx context.Context, err error) {
	if ctx.Err() == nil && a.cfg.Report != nil {
		a.cfg.Report(err)
	}
}

// Stop cancels every awaiter and waits for them.
func (a *Awaiters) Stop(ctx context.Context) error {
	a.mu.Lock()
	a.stopping = true
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
