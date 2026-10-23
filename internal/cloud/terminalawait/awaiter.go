// Package terminalawait is the cloud controller's leader-owned awaiter: it waits
// for each engine run to close in Temporal and fires the terminal-hook frame
// (internal/terminalhooks/frame) with the run's outcome. It is the cloud
// counterpart of the deleted daemon awaitInto (#7783).
//
// An awaiter is bound to one lease epoch's handle and stops with it. Hooks may
// fire more than once for a run (a successor re-fires what a crashed leader
// began), so every hook must be idempotent; the frame already requires it of
// Finalize. A run is picked up while it is open, and also after it closes while
// it still holds claims, so a leader that died between the close and the hooks
// does not leave the run's claims to expire. A failed attempt (an unreachable
// Temporal, an unidentifiable run) is reported and retried on the next Tick; it
// never blocks readiness.
package terminalawait

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
	// Hooks builds the frame for one run's identity.
	Hooks func(handle *lease.Handle, id Identity) *frame.Hooks
	// MaxConcurrent bounds simultaneous awaiters (default 256); further runs
	// wait for the next Tick.
	MaxConcurrent int
	Report        func(error)
}

// Awaiters is the epoch's awaiter set (it satisfies epoch.Awaiters).
type Awaiters struct {
	cfg    Config
	handle *lease.Handle

	mu       sync.Mutex
	running  map[string]struct{}
	cancel   context.CancelFunc
	ctx      context.Context
	stopping bool
	wg       sync.WaitGroup
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
	return &Awaiters{cfg: cfg, handle: handle, running: map[string]struct{}{}}, nil
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
	for _, run := range runs {
		a.start(run)
	}
	return nil
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
	a.cfg.Hooks(a.handle, id).Run(ctx, out)
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
