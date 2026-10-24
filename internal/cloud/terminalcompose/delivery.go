package terminalcompose

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
	"github.com/goobers/goobers/internal/terminalhooks/providerposter"
	"github.com/goobers/goobers/internal/terminalreport"
)

// Delivery is the epoch's own budgeted loop for the terminal-report outbox and
// the blocked-record self-heal. It runs apart from the awaiter, so a slow
// provider can delay a report but never the detection of a terminal. Each pass is
// throttled and jittered, bounded by a delivery budget, and fenced by the epoch's
// handle through the stores.
type Delivery struct {
	deps   Deps
	outbox terminalreportpg.Outbox
	blocks blockedrecordspg.Store

	every  time.Duration
	budget int
	heal   int
	maxLag time.Duration

	mu       sync.Mutex
	next     time.Time
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
	// Healthy is false when the oldest report has waited longer than the bound, or
	// the last pass failed to read the backlog.
	Healthy bool
}

func newDelivery(deps Deps, outbox terminalreportpg.Outbox, blocks blockedrecordspg.Store) *Delivery {
	t := deps.Tuning
	d := &Delivery{deps: deps, outbox: outbox, blocks: blocks, every: t.DeliveryEvery, budget: t.DeliveryBudget, heal: t.HealLimit, maxLag: t.MaxOutboxLag}
	if d.every <= 0 {
		d.every = 5 * time.Second
	}
	if d.budget <= 0 {
		d.budget = 20
	}
	if d.heal <= 0 {
		d.heal = 20
	}
	if d.maxLag <= 0 {
		d.maxLag = 15 * time.Minute
	}
	d.snapshot = Status{Healthy: true}
	return d
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
	d.mu.Unlock()

	var failure error
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
	apply := func(ctx context.Context, e terminalreport.Effect) error {
		return e.Apply(ctx, providerposter.Poster{Writer: d.deps.Writer, RunID: e.RunID})
	}
	if err := note(d.outbox.Reconcile(ctx, d.budget, apply)); err != nil {
		return err
	}
	if _, err := d.blocks.Heal(ctx, d.deps.Handle, d.heal); err != nil {
		if err := note(fmt.Errorf("heal blocked records: %w", err)); err != nil {
			return err
		}
	}
	backlog, err := d.outbox.Backlog(ctx)
	if err := note(err); err != nil {
		return err
	}
	d.mu.Lock()
	d.snapshot = Status{
		Pending: backlog.Pending, OldestAge: backlog.Oldest, LastPass: d.deps.Now(), LastError: failure,
		Healthy: failure == nil || backlog.Oldest <= d.maxLag,
	}
	if backlog.Oldest > d.maxLag {
		d.snapshot.Healthy = false
	}
	d.mu.Unlock()
	return nil
}
