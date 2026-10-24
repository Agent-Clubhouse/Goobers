package terminalcompose

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
	"github.com/goobers/goobers/internal/terminalhooks/providerposter"
	"github.com/goobers/goobers/internal/terminalreport"
)

// DefaultHealEvery is how often the blocked-record self-heal verifies items with
// the provider. It is minutes, not the delivery cadence: a pass reads the provider
// for each item it checks, outside the scheduler's own accounting, so a fast heal
// loop would spend the quota the scheduler gates on. Records are also verified at
// the moment a block would close a cycle (blockedrecordspg.Store.RecordBlock).
const DefaultHealEvery = 5 * time.Minute

// The narrow views of the stores the loop drives, so the loop is tested without a
// database.
type (
	reconciler interface {
		Reconcile(ctx context.Context, budget int, apply func(context.Context, terminalreport.Effect) error) error
	}
	backlogReader interface {
		Backlog(ctx context.Context) (terminalreportpg.Backlog, error)
	}
	healer interface {
		Heal(ctx context.Context, handle *lease.Handle, limit int) (int, error)
		Sweep(ctx context.Context, handle *lease.Handle, opt blockedrecordspg.SweepOptions) (blockedrecordspg.SweepResult, error)
		Deferrals(ctx context.Context, handle *lease.Handle) (int, error)
	}
)

// Delivery is the epoch's own budgeted loop for the terminal-report outbox and
// the blocked-record self-heal. It runs apart from the awaiter, so a slow
// provider can delay a report but never the detection of a terminal. Each pass is
// throttled and jittered, bounded by a delivery budget, and fenced by the epoch's
// handle through the stores.
type Delivery struct {
	deps      Deps
	reconcile reconciler
	backlog   backlogReader
	heal      healer
	apply     func(context.Context, terminalreport.Effect) error
	escalate  blockedrecordspg.EscalateFunc

	every     time.Duration
	healEvery time.Duration
	budget    int
	healLimit int
	maxLag    time.Duration

	mu       sync.Mutex
	next     time.Time
	healNext time.Time
	snapshot Status
}

// Status is the delivery loop's last observation, for readiness and the lag
// metric.
type Status struct {
	// Pending is the undelivered terminal reports; OldestAge how long the oldest
	// has waited (database clock).
	Pending   int
	OldestAge time.Duration
	// LastPass is when the loop last completed; LastError the last failure.
	LastPass  time.Time
	LastError error
	// DeferredCycles is the blocked-record cycles awaiting re-verification.
	DeferredCycles int
	// Healthy is false when the oldest report has waited longer than the bound, or
	// the backlog could not be read: an unreadable outbox is not a healthy one.
	Healthy bool
}

func newDelivery(deps Deps, outbox terminalreportpg.Outbox, blocks blockedrecordspg.Store, escalate blockedrecordspg.EscalateFunc) *Delivery {
	d := &Delivery{deps: deps, reconcile: outbox, backlog: outbox, heal: blocks, escalate: escalate}
	d.apply = func(ctx context.Context, e terminalreport.Effect) error {
		return e.Apply(ctx, providerposter.Poster{Writer: deps.Writer, RunID: e.RunID})
	}
	d.configure()
	return d
}

func (d *Delivery) configure() {
	t := d.deps.Tuning
	d.every, d.healEvery, d.budget, d.healLimit, d.maxLag = t.DeliveryEvery, t.HealEvery, t.DeliveryBudget, t.HealLimit, t.MaxOutboxLag
	if d.every <= 0 {
		d.every = 5 * time.Second
	}
	if d.healEvery <= 0 {
		d.healEvery = DefaultHealEvery
	}
	if d.budget <= 0 {
		d.budget = 20
	}
	if d.healLimit <= 0 {
		d.healLimit = 20
	}
	if d.maxLag <= 0 {
		d.maxLag = 15 * time.Minute
	}
	d.snapshot = Status{Healthy: true}
}

// Status returns the last observation.
func (d *Delivery) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot
}

// Tick runs one pass when it is due. It returns only an error that means the
// epoch's authority is gone; every other failure is reported and retried next
// pass, because a tick error ends the epoch.
func (d *Delivery) Tick(ctx context.Context) error {
	now := d.deps.Now()
	d.mu.Lock()
	if now.Before(d.next) {
		d.mu.Unlock()
		return nil
	}
	d.next = now.Add(d.every + time.Duration(rand.Int64N(int64(d.every)/5+1)) - d.every/10)
	healDue := !now.Before(d.healNext)
	if healDue {
		d.healNext = now.Add(d.healEvery)
	}
	d.mu.Unlock()

	var failure error
	// note reports a failure and keeps going; only a lost lease ends the pass.
	note := func(err error) error {
		if err == nil {
			return nil
		}
		if epoch.IsStale(err) {
			return err
		}
		d.deps.Report(err)
		failure = err
		return nil
	}
	if err := note(d.reconcile.Reconcile(ctx, d.budget, d.apply)); err != nil {
		return err
	}
	if healDue {
		if _, err := d.heal.Heal(ctx, d.deps.Handle, d.healLimit); err != nil {
			if err := note(fmt.Errorf("heal blocked records: %w", err)); err != nil {
				return err
			}
		}
		// The sweep is the driver of every deferred cycle: it re-checks the ones
		// that have fallen due, and escalates or drops each.
		if d.deps.Escalator != nil {
			opt := blockedrecordspg.SweepOptions{Bound: d.deps.Tuning.DeferralBound, MaxCycles: d.deps.Tuning.SweepCycles, MaxMembers: d.deps.Tuning.SweepMembers, Escalate: d.escalate}
			if _, err := d.heal.Sweep(ctx, d.deps.Handle, opt); err != nil {
				if err := note(fmt.Errorf("sweep deferred cycles: %w", err)); err != nil {
					return err
				}
			}
		}
	}
	deferred, deferErr := d.heal.Deferrals(ctx, d.deps.Handle)
	if err := note(deferErr); err != nil {
		return err
	}
	backlog, readErr := d.backlog.Backlog(ctx)
	if err := note(readErr); err != nil {
		return err
	}
	d.mu.Lock()
	d.snapshot = Status{
		Pending: backlog.Pending, OldestAge: backlog.Oldest, LastPass: d.deps.Now(), LastError: failure, DeferredCycles: deferred,
		Healthy: readErr == nil && backlog.Oldest <= d.maxLag,
	}
	d.mu.Unlock()
	return nil
}
