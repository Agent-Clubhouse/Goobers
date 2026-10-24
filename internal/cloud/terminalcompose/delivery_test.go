package terminalcompose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

type fakeLoop struct {
	sweeps       int
	sweepErr     error
	sweepResult  blockedrecordspg.SweepResult
	sweepOpt     blockedrecordspg.SweepOptions
	deferred     int
	budgets      []int
	heals        int
	healLimits   []int
	reconcileErr error
	healErr      error
	backlog      terminalreportpg.Backlog
	backlogErr   error
	refusedReads int
	refusedErr   error
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

func (f *fakeLoop) Sweep(_ context.Context, _ *lease.Handle, opt blockedrecordspg.SweepOptions) (blockedrecordspg.SweepResult, error) {
	f.sweeps++
	f.sweepOpt = opt
	return f.sweepResult, f.sweepErr
}

func (f *fakeLoop) Deferrals(context.Context, *lease.Handle) (int, error) { return f.deferred, nil }
func (f *fakeLoop) Refused(context.Context, *lease.Handle) ([]string, error) {
	f.refusedReads++
	if f.refusedErr != nil {
		return nil, f.refusedErr
	}
	return f.sweepResult.Refused, nil
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

// The sweep drives every deferred cycle, so it runs with the heal (its slow
// cadence) and only when there is something to escalate with.
func TestTheSweepRunsWithTheHealAndOnlyWithAnEscalator(t *testing.T) {
	f, c := &fakeLoop{deferred: 2}, &clock{now: time.Unix(1000, 0)}
	d, _ := newLoop(f, c, Tuning{DeferralBound: 20 * time.Minute, SweepCycles: 3, SweepMembers: 4})
	d.deps.Escalator = poster{}
	d.escalate = func(context.Context, blockedcycle.Result, blockedrecordspg.Deferral) error { return nil }
	if err := d.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.sweeps != 1 || f.sweepOpt.Bound != 20*time.Minute || f.sweepOpt.MaxCycles != 3 || f.sweepOpt.MaxMembers != 4 || f.sweepOpt.Escalate == nil {
		t.Fatalf("sweeps %d options %+v; want one pass with the configured bounds and an escalator", f.sweeps, f.sweepOpt)
	}
	if got := d.Status().DeferredCycles; got != 2 {
		t.Fatalf("status deferred cycles = %d, want 2", got)
	}
	c.now = c.now.Add(2 * time.Second) // delivery is due again, the heal is not
	if err := d.Tick(t.Context()); err != nil || f.sweeps != 1 {
		t.Fatalf("sweeps = %d after a delivery-only pass (err %v); the sweep follows the heal cadence", f.sweeps, err)
	}
	// Without an escalator there is no driver: the sweep does not run, and the
	// store (Defer unset) escalates unverifiable cycles itself.
	f2 := &fakeLoop{}
	d2, _ := newLoop(f2, &clock{now: time.Unix(1000, 0)}, Tuning{})
	if err := d2.Tick(t.Context()); err != nil || f2.sweeps != 0 {
		t.Fatalf("sweeps without an escalator = %d, err %v", f2.sweeps, err)
	}
	// A lost lease in the sweep ends the epoch; other failures are reported.
	f3 := &fakeLoop{sweepErr: lease.ErrStale}
	d3, _ := newLoop(f3, &clock{now: time.Unix(1000, 0)}, Tuning{})
	d3.deps.Escalator = poster{}
	if err := d3.Tick(t.Context()); err == nil {
		t.Fatal("a stale fence in the sweep did not end the epoch")
	}
	f4 := &fakeLoop{sweepErr: errors.New("provider down")}
	d4, reported := newLoop(f4, &clock{now: time.Unix(1000, 0)}, Tuning{})
	d4.deps.Escalator = poster{}
	if err := d4.Tick(t.Context()); err != nil || len(*reported) == 0 {
		t.Fatalf("sweep failure: err %v reported %v; want reported, epoch kept", err, *reported)
	}
}

type poster struct{}

func (poster) UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}

type memLog struct{ events []journal.Event }

func (l *memLog) AppendBestEffort(ev journal.Event) { l.events = append(l.events, ev) }

// A cycle whose escalation the provider permanently refused is no longer retried by
// the sweep, so it must be loud: a journaled error per cycle and a standing status.
func TestAPermanentlyRefusedEscalationIsJournaledAndStaysVisible(t *testing.T) {
	f, c := &fakeLoop{sweepResult: blockedrecordspg.SweepResult{NewlyRefused: []string{"2", "7"}, Refused: []string{"2", "7"}}, sweepErr: blockedrecordspg.ErrEscalationRefused}, &clock{now: time.Unix(1000, 0)}
	d, reported := newLoop(f, c, Tuning{})
	d.deps.Escalator = poster{}
	log := &memLog{}
	d.deps.Log = log
	if err := d.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := d.Status()
	if len(status.RefusedCycles) != 2 || status.RefusedCycles[0] != "2" {
		t.Fatalf("refused cycles = %v, want both named", status.RefusedCycles)
	}
	var journaled int
	for _, ev := range log.events {
		if ev.Runner["kind"] == "blocked_cycle_escalation_refused" {
			journaled++
		}
	}
	if journaled != 2 || len(*reported) == 0 {
		t.Fatalf("journaled %d refusals, reported %v; want one journaled error per cycle and the failure reported", journaled, *reported)
	}
	// It stays visible on later passes without being journaled again, and clears
	// when the store reports the cycle resolved.
	f.sweepResult, f.sweepErr = blockedrecordspg.SweepResult{Refused: []string{"2", "7"}}, nil
	c.now = c.now.Add(time.Hour)
	if err := d.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(d.Status().RefusedCycles) != 2 || len(log.events) != journaled {
		t.Fatalf("later pass: refused %v, journaled events %d; want standing and no re-journal", d.Status().RefusedCycles, len(log.events))
	}
	f.sweepResult = blockedrecordspg.SweepResult{}
	c.now = c.now.Add(time.Hour)
	if err := d.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := d.Status().RefusedCycles; len(got) != 0 {
		t.Fatalf("refused after resolution = %v, want none", got)
	}
}

// The refused scan is a table read: it runs on the heal cadence, not every pass,
// and the last answer stands between reads.
func TestTheRefusedScanRunsOnTheHealCadenceNotEveryPass(t *testing.T) {
	f, c := &fakeLoop{sweepResult: blockedrecordspg.SweepResult{Refused: []string{"2"}}}, &clock{now: time.Unix(1000, 0)}
	d, _ := newLoop(f, c, Tuning{})
	for range 5 {
		c.now = c.now.Add(time.Minute) // passes are due every few seconds; heal every 5 minutes
		if err := d.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f.refusedReads != 1 {
		t.Fatalf("refused scans = %d over 5 passes inside one heal window, want 1", f.refusedReads)
	}
	if got := d.Status().RefusedCycles; len(got) != 1 {
		t.Fatalf("refused between scans = %v, want the last answer to stand", got)
	}
}

// A failed refused-cycle read keeps the last known list and marks it unknown, so
// readiness can fail closed; the next good read clears the mark.
func TestAFailedRefusedReadKeepsTheLastListAndMarksItUnknown(t *testing.T) {
	f, c := &fakeLoop{sweepResult: blockedrecordspg.SweepResult{Refused: []string{"2"}}}, &clock{now: time.Unix(1000, 0)}
	d, _ := newLoop(f, c, Tuning{})
	tick := func() Status {
		t.Helper()
		c.now = c.now.Add(time.Hour) // every pass is a heal pass
		if err := d.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		return d.Status()
	}
	if s := tick(); len(s.RefusedCycles) != 1 || s.RefusedUnknown {
		t.Fatalf("status = %+v, want one refused and a known answer", s)
	}
	f.refusedErr = errors.New("database unavailable")
	if s := tick(); len(s.RefusedCycles) != 1 || !s.RefusedUnknown {
		t.Fatalf("status after a failed read = %+v, want the last list kept and the answer marked unknown", s)
	}
	f.refusedErr = nil
	f.sweepResult = blockedrecordspg.SweepResult{}
	if s := tick(); len(s.RefusedCycles) != 0 || s.RefusedUnknown {
		t.Fatalf("status after recovery = %+v, want none and known", s)
	}
}

// Comments are keyed by cycle, item and position, not text: a retry whose notes
// changed still lands on the same key.
func TestCycleCommentsAreKeyedByPositionNotByText(t *testing.T) {
	var keys []string
	inner := keyRecorder{keys: &keys}
	post := func(comments ...string) []string {
		keys = nil
		p := newKeyedPoster(inner, "cycle-abc")
		for _, item := range []string{"1", "2"} {
			for _, comment := range comments {
				if _, err := p.UpdateWorkItem(t.Context(), providers.UpdateWorkItemRequest{ID: item, Comment: comment}); err != nil {
					t.Fatal(err)
				}
			}
		}
		return slices.Clone(keys)
	}
	first := post("report", "note: #2 rate limited")
	retry := post("report", "note: #2 provider unavailable")
	if !slices.Equal(first, retry) {
		t.Fatalf("keys changed with the comment text: %v vs %v", first, retry)
	}
	want := []string{"cycle-abc/1/0", "cycle-abc/1/1", "cycle-abc/2/0", "cycle-abc/2/1"}
	if !slices.Equal(first, want) {
		t.Fatalf("keys = %v, want %v", first, want)
	}
	// A caller's own key is respected; a comment-less request is untouched.
	keys = nil
	p := newKeyedPoster(inner, "cycle-abc")
	_, _ = p.UpdateWorkItem(t.Context(), providers.UpdateWorkItemRequest{ID: "1", Comment: "c", IdempotencyKey: "mine"})
	_, _ = p.UpdateWorkItem(t.Context(), providers.UpdateWorkItemRequest{ID: "1", AddLabels: []string{"x"}})
	if !slices.Equal(keys, []string{"mine", ""}) {
		t.Fatalf("keys = %v, want the caller's key kept and no key invented for a label-only request", keys)
	}
}

type keyRecorder struct{ keys *[]string }

func (k keyRecorder) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	*k.keys = append(*k.keys, req.IdempotencyKey)
	return providers.WorkItem{}, nil
}
