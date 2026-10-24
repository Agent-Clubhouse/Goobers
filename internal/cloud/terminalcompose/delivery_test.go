package terminalcompose

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
	"github.com/goobers/goobers/internal/terminalreport"
)

type fakeLoop struct {
	budgets      []int
	heals        int
	healLimits   []int
	reconcileErr error
	healErr      error
	backlog      terminalreportpg.Backlog
	backlogErr   error
}

func (f *fakeLoop) Reconcile(_ context.Context, budget int, _ func(context.Context, terminalreport.Effect) error) error {
	f.budgets = append(f.budgets, budget)
	return f.reconcileErr
}

func (f *fakeLoop) Heal(_ context.Context, _ *lease.Handle, limit int) (int, error) {
	f.heals++
	f.healLimits = append(f.healLimits, limit)
	return 0, f.healErr
}

func (f *fakeLoop) Backlog(context.Context) (terminalreportpg.Backlog, error) {
	return f.backlog, f.backlogErr
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newLoop(f *fakeLoop, c *clock, tune Tuning) (*Delivery, *[]error) {
	var reported []error
	d := &Delivery{deps: Deps{Handle: &lease.Handle{InstanceID: "i", Epoch: 1}, Now: c.Now, Tuning: tune, Report: func(err error) { reported = append(reported, err) }}, reconcile: f, backlog: f, heal: f}
	d.configure()
	return d, &reported
}

func TestDeliveryReconcilesWithItsConfiguredBudget(t *testing.T) {
	f, c := &fakeLoop{}, &clock{now: time.Unix(1000, 0)}
	d, _ := newLoop(f, c, Tuning{DeliveryBudget: 7})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.budgets) != 1 || f.budgets[0] != 7 {
		t.Fatalf("reconcile budgets = %v, want one pass at 7", f.budgets)
	}
}

// The heal verifies items with the provider, so it runs on its own slow cadence,
// not on every delivery pass.
func TestHealRunsOnItsOwnSlowCadence(t *testing.T) {
	f, c := &fakeLoop{}, &clock{now: time.Unix(1000, 0)}
	d, _ := newLoop(f, c, Tuning{DeliveryEvery: time.Second, HealEvery: 10 * time.Minute, HealLimit: 3})
	pass := func() {
		c.now = c.now.Add(2 * time.Second) // always past the delivery interval and its jitter
		if err := d.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for range 50 {
		pass()
	}
	if len(f.budgets) != 50 {
		t.Fatalf("delivery passes = %d, want 50", len(f.budgets))
	}
	if f.heals != 1 {
		t.Fatalf("heals in 100s of delivery = %d, want 1 (the first pass only)", f.heals)
	}
	c.now = c.now.Add(10 * time.Minute)
	pass()
	if f.heals != 2 || f.healLimits[0] != 3 {
		t.Fatalf("heals after the heal interval = %d (limits %v), want 2 at limit 3", f.heals, f.healLimits)
	}
	if DefaultHealEvery < time.Minute {
		t.Fatalf("DefaultHealEvery = %v: provider reads outside the quota ledger must be slow", DefaultHealEvery)
	}
}

// A tick error ends the epoch, so only a lost lease may surface; any other
// failure is reported and the loop carries on.
func TestOnlyALostLeaseEndsTheEpoch(t *testing.T) {
	for name, tc := range map[string]struct {
		f    fakeLoop
		ends bool
	}{
		"reconcile stale":      {fakeLoop{reconcileErr: fmt.Errorf("claim: %w", lease.ErrStale)}, true},
		"reconcile self-fence": {fakeLoop{reconcileErr: lease.ErrSelfFenced}, true},
		"heal stale":           {fakeLoop{healErr: lease.ErrStale}, true},
		"backlog stale":        {fakeLoop{backlogErr: lease.ErrStale}, true},
		"provider failure":     {fakeLoop{reconcileErr: errors.New("provider down")}, false},
		"heal failure":         {fakeLoop{healErr: errors.New("provider down")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, c := tc.f, &clock{now: time.Unix(1000, 0)}
			d, reported := newLoop(&f, c, Tuning{})
			err := d.Tick(t.Context())
			if tc.ends != (err != nil) {
				t.Fatalf("Tick = %v, want ends=%v", err, tc.ends)
			}
			if !tc.ends && len(*reported) == 0 {
				t.Fatal("a failure was swallowed without a report")
			}
		})
	}
}

func TestHealthIsFalseOnLagAndOnAnUnreadableBacklog(t *testing.T) {
	c := &clock{now: time.Unix(1000, 0)}
	for name, tc := range map[string]struct {
		f       fakeLoop
		healthy bool
	}{
		"empty outbox":     {fakeLoop{}, true},
		"within the bound": {fakeLoop{backlog: terminalreportpg.Backlog{Pending: 2, Oldest: time.Minute}}, true},
		"lagging":          {fakeLoop{backlog: terminalreportpg.Backlog{Pending: 2, Oldest: time.Hour}}, false},
		"unreadable":       {fakeLoop{backlogErr: errors.New("connection refused")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := tc.f
			d, _ := newLoop(&f, c, Tuning{MaxOutboxLag: 15 * time.Minute})
			if err := d.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			status := d.Status()
			if status.Healthy != tc.healthy {
				t.Fatalf("healthy = %v, want %v (status %+v)", status.Healthy, tc.healthy, status)
			}
			if tc.f.backlog.Pending != 0 && status.Pending != tc.f.backlog.Pending {
				t.Fatalf("pending = %d, want %d", status.Pending, tc.f.backlog.Pending)
			}
		})
	}
	// Before any pass the loop reports healthy: a standby has nothing owed.
	d, _ := newLoop(&fakeLoop{}, c, Tuning{})
	if !d.Status().Healthy {
		t.Fatal("a loop that has not run reported unhealthy")
	}
}
